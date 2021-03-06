package transaction

import (
	"bytes"
	"errors"
	"math/big"

	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/check"
	"github.com/ElrondNetwork/elrond-go/data"
	"github.com/ElrondNetwork/elrond-go/data/receipt"
	"github.com/ElrondNetwork/elrond-go/data/smartContractResult"
	"github.com/ElrondNetwork/elrond-go/data/state"
	"github.com/ElrondNetwork/elrond-go/data/transaction"
	"github.com/ElrondNetwork/elrond-go/hashing"
	"github.com/ElrondNetwork/elrond-go/marshal"
	"github.com/ElrondNetwork/elrond-go/process"
	"github.com/ElrondNetwork/elrond-go/sharding"
	vmcommon "github.com/ElrondNetwork/elrond-vm-common"
)

var _ process.TransactionProcessor = (*txProcessor)(nil)

// txProcessor implements TransactionProcessor interface and can modify account states according to a transaction
type txProcessor struct {
	*baseTxProcessor
	txFeeHandler     process.TransactionFeeHandler
	txTypeHandler    process.TxTypeHandler
	receiptForwarder process.IntermediateTransactionHandler
	badTxForwarder   process.IntermediateTransactionHandler
	argsParser       process.ArgumentsParser
	scrForwarder     process.IntermediateTransactionHandler
	signMarshalizer  marshal.Marshalizer
}

// NewTxProcessor creates a new txProcessor engine
func NewTxProcessor(
	accounts state.AccountsAdapter,
	hasher hashing.Hasher,
	pubkeyConv core.PubkeyConverter,
	marshalizer marshal.Marshalizer,
	signMarshalizer marshal.Marshalizer,
	shardCoordinator sharding.Coordinator,
	scProcessor process.SmartContractProcessor,
	txFeeHandler process.TransactionFeeHandler,
	txTypeHandler process.TxTypeHandler,
	economicsFee process.FeeHandler,
	receiptForwarder process.IntermediateTransactionHandler,
	badTxForwarder process.IntermediateTransactionHandler,
	argsParser process.ArgumentsParser,
	scrForwarder process.IntermediateTransactionHandler,
) (*txProcessor, error) {

	if check.IfNil(accounts) {
		return nil, process.ErrNilAccountsAdapter
	}
	if check.IfNil(hasher) {
		return nil, process.ErrNilHasher
	}
	if check.IfNil(pubkeyConv) {
		return nil, process.ErrNilPubkeyConverter
	}
	if check.IfNil(marshalizer) {
		return nil, process.ErrNilMarshalizer
	}
	if check.IfNil(shardCoordinator) {
		return nil, process.ErrNilShardCoordinator
	}
	if check.IfNil(scProcessor) {
		return nil, process.ErrNilSmartContractProcessor
	}
	if check.IfNil(txFeeHandler) {
		return nil, process.ErrNilUnsignedTxHandler
	}
	if check.IfNil(txTypeHandler) {
		return nil, process.ErrNilTxTypeHandler
	}
	if check.IfNil(economicsFee) {
		return nil, process.ErrNilEconomicsFeeHandler
	}
	if check.IfNil(receiptForwarder) {
		return nil, process.ErrNilReceiptHandler
	}
	if check.IfNil(badTxForwarder) {
		return nil, process.ErrNilBadTxHandler
	}
	if check.IfNil(argsParser) {
		return nil, process.ErrNilArgumentParser
	}
	if check.IfNil(scrForwarder) {
		return nil, process.ErrNilIntermediateTransactionHandler
	}
	if check.IfNil(signMarshalizer) {
		return nil, process.ErrNilMarshalizer
	}

	baseTxProcess := &baseTxProcessor{
		accounts:         accounts,
		shardCoordinator: shardCoordinator,
		pubkeyConv:       pubkeyConv,
		economicsFee:     economicsFee,
		hasher:           hasher,
		marshalizer:      marshalizer,
		scProcessor:      scProcessor,
	}

	return &txProcessor{
		baseTxProcessor:  baseTxProcess,
		txFeeHandler:     txFeeHandler,
		txTypeHandler:    txTypeHandler,
		receiptForwarder: receiptForwarder,
		badTxForwarder:   badTxForwarder,
		argsParser:       argsParser,
		scrForwarder:     scrForwarder,
		signMarshalizer:  signMarshalizer,
	}, nil
}

// ProcessTransaction modifies the account states in respect with the transaction data
func (txProc *txProcessor) ProcessTransaction(tx *transaction.Transaction) (vmcommon.ReturnCode, error) {
	if check.IfNil(tx) {
		return 0, process.ErrNilTransaction
	}

	acntSnd, acntDst, err := txProc.getAccounts(tx.SndAddr, tx.RcvAddr)
	if err != nil {
		return 0, err
	}

	process.DisplayProcessTxDetails(
		"ProcessTransaction: sender account details",
		acntSnd,
		tx,
		txProc.pubkeyConv,
	)

	err = txProc.checkTxValues(tx, acntSnd, acntDst)
	if err != nil {
		if errors.Is(err, process.ErrInsufficientFunds) {
			receiptErr := txProc.executingFailedTransaction(tx, acntSnd, err)
			if receiptErr != nil {
				return 0, receiptErr
			}
		}

		if errors.Is(err, process.ErrUserNameDoesNotMatchInCrossShardTx) {
			errProcessIfErr := txProc.processIfTxErrorCrossShard(tx, err.Error())
			if errProcessIfErr != nil {
				return 0, errProcessIfErr
			}
			return vmcommon.UserError, nil
		}
		return vmcommon.UserError, err
	}

	txType := txProc.txTypeHandler.ComputeTransactionType(tx)
	switch txType {
	case process.MoveBalance:
		err = txProc.processMoveBalance(tx, tx.SndAddr, tx.RcvAddr)
		return vmcommon.Ok, err
	case process.SCDeployment:
		return txProc.processSCDeployment(tx, tx.SndAddr)
	case process.SCInvoking:
		return txProc.processSCInvoking(tx, tx.SndAddr, tx.RcvAddr)
	case process.BuiltInFunctionCall:
		return txProc.processSCInvoking(tx, tx.SndAddr, tx.RcvAddr)
	case process.RelayedTx:
		return txProc.processRelayedTx(tx, tx.SndAddr, tx.RcvAddr)
	}

	return vmcommon.UserError, process.ErrWrongTransaction
}

func (txProc *txProcessor) executingFailedTransaction(
	tx *transaction.Transaction,
	acntSnd state.UserAccountHandler,
	txError error,
) error {
	if check.IfNil(acntSnd) {
		return nil
	}

	txFee := txProc.economicsFee.ComputeFee(tx)
	err := acntSnd.SubFromBalance(txFee)
	if err != nil {
		return err
	}

	acntSnd.IncreaseNonce(1)
	err = txProc.badTxForwarder.AddIntermediateTransactions([]data.TransactionHandler{tx})
	if err != nil {
		return err
	}

	txHash, err := core.CalculateHash(txProc.marshalizer, txProc.hasher, tx)
	if err != nil {
		return err
	}

	rpt := &receipt.Receipt{
		Value:   big.NewInt(0).Set(txFee),
		SndAddr: tx.SndAddr,
		Data:    []byte(txError.Error()),
		TxHash:  txHash,
	}

	err = txProc.receiptForwarder.AddIntermediateTransactions([]data.TransactionHandler{rpt})
	if err != nil {
		return err
	}

	txProc.txFeeHandler.ProcessTransactionFee(txFee, big.NewInt(0), txHash)

	err = txProc.accounts.SaveAccount(acntSnd)
	if err != nil {
		return err
	}

	return process.ErrFailedTransaction
}

func (txProc *txProcessor) createReceiptWithReturnedGas(txHash []byte, tx *transaction.Transaction, acntSnd state.UserAccountHandler) error {
	if check.IfNil(acntSnd) {
		return nil
	}
	if core.IsSmartContractAddress(tx.RcvAddr) {
		return nil
	}

	totalProvided := big.NewInt(0)
	totalProvided.Mul(big.NewInt(0).SetUint64(tx.GasPrice), big.NewInt(0).SetUint64(tx.GasLimit))

	actualCost := txProc.economicsFee.ComputeFee(tx)
	refundValue := big.NewInt(0).Sub(totalProvided, actualCost)

	zero := big.NewInt(0)
	if refundValue.Cmp(zero) == 0 {
		return nil
	}

	rpt := &receipt.Receipt{
		Value:   big.NewInt(0).Set(refundValue),
		SndAddr: tx.SndAddr,
		Data:    []byte("refundedGas"),
		TxHash:  txHash,
	}

	err := txProc.receiptForwarder.AddIntermediateTransactions([]data.TransactionHandler{rpt})
	if err != nil {
		return err
	}

	return nil
}

func (txProc *txProcessor) processTxFee(
	tx *transaction.Transaction,
	acntSnd, acntDst state.UserAccountHandler,
) (*big.Int, error) {
	if check.IfNil(acntSnd) {
		return big.NewInt(0), nil
	}

	cost := txProc.economicsFee.ComputeFee(tx)

	isCrossShardSCCall := check.IfNil(acntDst) && len(tx.GetData()) > 0 && core.IsSmartContractAddress(tx.GetRcvAddr())
	if isCrossShardSCCall {
		totalCost := big.NewInt(0).Mul(big.NewInt(0).SetUint64(tx.GetGasLimit()), big.NewInt(0).SetUint64(tx.GetGasPrice()))
		err := acntSnd.SubFromBalance(totalCost)
		if err != nil {
			return nil, err
		}
	} else {
		err := acntSnd.SubFromBalance(cost)
		if err != nil {
			return nil, err
		}
	}

	return cost, nil
}

func (txProc *txProcessor) checkIfValidTxToMetaChain(
	tx *transaction.Transaction,
	acntSnd state.UserAccountHandler,
	adrDst []byte,
) error {

	destShardId := txProc.shardCoordinator.ComputeId(adrDst)
	if destShardId != core.MetachainShardId {
		return nil
	}

	// it is not allowed to send transactions to metachain if those are not of type smart contract
	if len(tx.GetData()) == 0 {
		return txProc.executingFailedTransaction(tx, acntSnd, process.ErrInvalidMetaTransaction)
	}

	return nil
}

func (txProc *txProcessor) processMoveBalance(
	tx *transaction.Transaction,
	adrSrc, adrDst []byte,
) error {

	// getAccounts returns acntSrc not nil if the adrSrc is in the node shard, the same, acntDst will be not nil
	// if adrDst is in the node shard. If an error occurs it will be signaled in err variable.
	acntSrc, acntDst, err := txProc.getAccounts(adrSrc, adrDst)
	if err != nil {
		return err
	}

	err = txProc.checkIfValidTxToMetaChain(tx, acntSrc, adrDst)
	if err != nil {
		return err
	}

	txFee, err := txProc.processTxFee(tx, acntSrc, acntDst)
	if err != nil {
		return err
	}

	err = txProc.moveBalances(acntSrc, acntDst, tx.GetValue())
	if err != nil {
		return err
	}

	// is sender address in node shard
	if acntSrc != nil {
		acntSrc.IncreaseNonce(1)
	}

	txHash, err := core.CalculateHash(txProc.marshalizer, txProc.hasher, tx)
	if err != nil {
		return err
	}

	err = txProc.createReceiptWithReturnedGas(txHash, tx, acntSrc)
	if err != nil {
		return err
	}

	txProc.txFeeHandler.ProcessTransactionFee(txFee, big.NewInt(0), txHash)

	return txProc.saveAccounts(acntSrc, acntDst)
}

func (txProc *txProcessor) saveAccounts(acntSnd, acntDst state.AccountHandler) error {
	if !check.IfNil(acntSnd) {
		err := txProc.accounts.SaveAccount(acntSnd)
		if err != nil {
			return err
		}
	}

	if !check.IfNil(acntDst) {
		err := txProc.accounts.SaveAccount(acntDst)
		if err != nil {
			return err
		}
	}

	return nil
}

func (txProc *txProcessor) processSCDeployment(
	tx *transaction.Transaction,
	adrSrc []byte,
) (vmcommon.ReturnCode, error) {
	// getAccounts returns acntSrc not nil if the adrSrc is in the node shard, the same, acntDst will be not nil
	// if adrDst is in the node shard. If an error occurs it will be signaled in err variable.
	acntSrc, err := txProc.getAccountFromAddress(adrSrc)
	if err != nil {
		return 0, err
	}

	return txProc.scProcessor.DeploySmartContract(tx, acntSrc)
}

func (txProc *txProcessor) processSCInvoking(
	tx *transaction.Transaction,
	adrSrc, adrDst []byte,
) (vmcommon.ReturnCode, error) {
	// getAccounts returns acntSrc not nil if the adrSrc is in the node shard, the same, acntDst will be not nil
	// if adrDst is in the node shard. If an error occurs it will be signaled in err variable.
	acntSrc, acntDst, err := txProc.getAccounts(adrSrc, adrDst)
	if err != nil {
		return 0, err
	}

	return txProc.scProcessor.ExecuteSmartContractTransaction(tx, acntSrc, acntDst)
}

func (txProc *txProcessor) moveBalances(
	acntSrc, acntDst state.UserAccountHandler,
	value *big.Int,
) error {
	// is sender address in node shard
	if !check.IfNil(acntSrc) {
		err := acntSrc.SubFromBalance(value)
		if err != nil {
			return err
		}
	}

	// is receiver address in node shard
	if !check.IfNil(acntDst) {
		err := acntDst.AddToBalance(value)
		if err != nil {
			return err
		}
	}

	return nil
}

func (txProc *txProcessor) processRelayedTx(
	tx *transaction.Transaction,
	adrSrc, adrDst []byte,
) (vmcommon.ReturnCode, error) {

	_, args, err := txProc.argsParser.ParseCallData(string(tx.GetData()))
	if err != nil {
		return 0, err
	}

	relayerAcnt, acntDst, err := txProc.getAccounts(adrSrc, adrDst)
	if err != nil {
		return 0, err
	}

	if len(args) != 1 {
		return vmcommon.UserError, txProc.executingFailedTransaction(tx, relayerAcnt, process.ErrInvalidArguments)
	}

	userTx := &transaction.Transaction{}
	err = txProc.signMarshalizer.Unmarshal(userTx, args[0])
	if err != nil {
		return vmcommon.UserError, txProc.executingFailedTransaction(tx, relayerAcnt, err)
	}
	if !bytes.Equal(userTx.SndAddr, tx.RcvAddr) {
		return vmcommon.UserError, txProc.executingFailedTransaction(tx, relayerAcnt, process.ErrRelayedTxBeneficiaryDoesNotMatchReceiver)
	}
	if userTx.Value.Cmp(tx.Value) < 0 {
		return vmcommon.UserError, txProc.executingFailedTransaction(tx, relayerAcnt, process.ErrRelayedTxValueHigherThenUserTxValue)
	}

	totalFee, remainingFee := txProc.computeRelayedTxFees(tx)

	txHash, err := core.CalculateHash(txProc.marshalizer, txProc.hasher, tx)
	if err != nil {
		return 0, err
	}

	if !check.IfNil(relayerAcnt) {
		err = relayerAcnt.SubFromBalance(tx.GetValue())
		if err != nil {
			return 0, err
		}

		err = relayerAcnt.SubFromBalance(totalFee)
		if err != nil {
			return 0, err
		}

		relayerAcnt.IncreaseNonce(1)
		err = txProc.accounts.SaveAccount(relayerAcnt)
		if err != nil {
			return 0, err
		}

		txProc.txFeeHandler.ProcessTransactionFee(totalFee, big.NewInt(0), txHash)
	}

	if check.IfNil(acntDst) {
		return vmcommon.Ok, nil
	}

	err = acntDst.AddToBalance(tx.GetValue())
	if err != nil {
		return 0, err
	}

	err = acntDst.AddToBalance(remainingFee)
	if err != nil {
		return 0, err
	}

	err = txProc.accounts.SaveAccount(acntDst)
	if err != nil {
		return 0, err
	}

	return txProc.processUserTx(userTx, adrSrc, tx.Value, tx.Nonce, txHash)
}

func (txProc *txProcessor) computeRelayedTxFees(tx *transaction.Transaction) (*big.Int, *big.Int) {
	relayerGasLimit := txProc.economicsFee.ComputeGasLimit(tx)
	relayerFee := big.NewInt(0).Mul(big.NewInt(0).SetUint64(relayerGasLimit), big.NewInt(0).SetUint64(tx.GetGasPrice()))
	totalFee := big.NewInt(0).Mul(big.NewInt(0).SetUint64(tx.GetGasLimit()), big.NewInt(0).SetUint64(tx.GetGasPrice()))
	remainingFee := big.NewInt(0).Sub(totalFee, relayerFee)

	return totalFee, remainingFee
}

func (txProc *txProcessor) processUserTx(
	userTx *transaction.Transaction,
	relayerAdr []byte,
	relayedTxValue *big.Int,
	relayedNonce uint64,
	txHash []byte,
) (vmcommon.ReturnCode, error) {
	acntSnd, acntDst, err := txProc.getAccounts(userTx.SndAddr, userTx.RcvAddr)
	if err != nil {
		return 0, err
	}

	err = txProc.checkTxValues(userTx, acntSnd, acntDst)
	if err != nil {
		return vmcommon.UserError, txProc.executeFailedRelayedTransaction(
			userTx.SndAddr,
			relayerAdr,
			relayedTxValue,
			relayedNonce,
			txHash,
			err.Error())
	}

	scrFromTx := txProc.makeSCRFromUserTx(userTx, relayerAdr, relayedTxValue, txHash)

	returnCode := vmcommon.Ok
	txType := txProc.txTypeHandler.ComputeTransactionType(scrFromTx)
	switch txType {
	case process.MoveBalance:
		err = txProc.processMoveBalance(userTx, userTx.SndAddr, userTx.RcvAddr)
	case process.SCDeployment:
		returnCode, err = txProc.scProcessor.DeploySmartContract(scrFromTx, acntSnd)
	case process.SCInvoking:
		returnCode, err = txProc.scProcessor.ExecuteSmartContractTransaction(scrFromTx, acntSnd, acntDst)
	case process.BuiltInFunctionCall:
		returnCode, err = txProc.scProcessor.ExecuteSmartContractTransaction(scrFromTx, acntSnd, acntDst)
	default:
		err = process.ErrWrongTransaction
		return vmcommon.UserError, txProc.executeFailedRelayedTransaction(
			userTx.SndAddr,
			relayerAdr,
			relayedTxValue,
			relayedNonce,
			txHash,
			err.Error())
	}

	// coding error transaction is reverted completely by revert from txPreProcessor
	if err != nil {
		return 0, err
	}

	// no need to add the smart contract result From TX to the intermediate transactions in case of error
	// returning value is resolved inside smart contract processor or above by executeFailedRelayedTransaction
	if returnCode != vmcommon.Ok {
		return returnCode, nil
	}

	err = txProc.scrForwarder.AddIntermediateTransactions([]data.TransactionHandler{scrFromTx})
	if err != nil {
		return 0, err
	}

	return vmcommon.Ok, nil
}

func (txProc *txProcessor) makeSCRFromUserTx(
	tx *transaction.Transaction,
	relayerAdr []byte,
	relayedTxValue *big.Int,
	txHash []byte,
) *smartContractResult.SmartContractResult {
	scr := &smartContractResult.SmartContractResult{
		Nonce:          tx.Nonce,
		Value:          tx.Value,
		RcvAddr:        tx.RcvAddr,
		SndAddr:        tx.SndAddr,
		RelayerAddr:    relayerAdr,
		RelayedValue:   big.NewInt(0).Set(relayedTxValue),
		Data:           tx.Data,
		PrevTxHash:     txHash,
		OriginalTxHash: txHash,
		GasLimit:       tx.GasLimit,
		GasPrice:       tx.GasPrice,
		CallType:       vmcommon.DirectCall,
	}
	return scr
}

func (txProc *txProcessor) executeFailedRelayedTransaction(
	userAdr []byte,
	relayerAdr []byte,
	relayedTxValue *big.Int,
	relayedNonce uint64,
	originalTxHash []byte,
	errorMsg string,
) error {
	userAcnt, err := txProc.getAccountFromAddress(userAdr)
	if err != nil {
		return err
	}
	if check.IfNil(userAcnt) {
		return process.ErrNilAccountsAdapter
	}
	err = userAcnt.SubFromBalance(relayedTxValue)
	if err != nil {
		return err
	}

	scrForRelayer := &smartContractResult.SmartContractResult{
		Nonce:          relayedNonce,
		Value:          big.NewInt(0).Set(relayedTxValue),
		RcvAddr:        relayerAdr,
		SndAddr:        userAdr,
		OriginalTxHash: originalTxHash,
		ReturnMessage:  []byte(errorMsg),
	}

	relayerAcnt, err := txProc.getAccountFromAddress(relayerAdr)
	if err != nil {
		return err
	}

	if !check.IfNil(relayerAcnt) {
		err = relayerAcnt.AddToBalance(scrForRelayer.Value)
		if err != nil {
			return err
		}
	}

	err = txProc.scrForwarder.AddIntermediateTransactions([]data.TransactionHandler{scrForRelayer})
	if err != nil {
		return err
	}

	return nil
}

// IsInterfaceNil returns true if there is no value under the interface
func (txProc *txProcessor) IsInterfaceNil() bool {
	return txProc == nil
}
