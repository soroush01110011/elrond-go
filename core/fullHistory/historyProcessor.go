//go:generate protoc -I=proto -I=$GOPATH/src -I=$GOPATH/src/github.com/ElrondNetwork/protobuf/protobuf  --gogoslick_out=. historyTransaction.proto

package fullHistory

import (
	"errors"

	logger "github.com/ElrondNetwork/elrond-go-logger"
	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/check"
	"github.com/ElrondNetwork/elrond-go/data"
	"github.com/ElrondNetwork/elrond-go/data/block"
	"github.com/ElrondNetwork/elrond-go/hashing"
	"github.com/ElrondNetwork/elrond-go/marshal"
	"github.com/ElrondNetwork/elrond-go/storage"
)

var log = logger.GetOrCreate("core/fullHistory")

// HistoryProcessorArguments is the structure that store all components that are needed to a history processor
type HistoryProcessorArguments struct {
	SelfShardID     uint32
	HistoryStorer   storage.Storer
	HashEpochStorer storage.Storer
	Marshalizer     marshal.Marshalizer
	Hasher          hashing.Hasher
}

// HistoryTransactionsData is structure that stores information about history transactions
type HistoryTransactionsData struct {
	HeaderHash    []byte
	HeaderHandler data.HeaderHandler
	BodyHandler   data.BodyHandler
}

// HistoryTransactionWithEpoch is the structure for a history transaction that also contain epoch
type HistoryTransactionWithEpoch struct {
	Epoch uint32
	*HistoryTransaction
}

type historyProcessor struct {
	selfShardID   uint32
	historyStorer storage.Storer
	marshalizer   marshal.Marshalizer
	hasher        hashing.Hasher
	hashEpochProc hashEpochHandler
}

// NewHistoryProcessor will create a new instance of HistoryProcessor
func NewHistoryProcessor(arguments HistoryProcessorArguments) (*historyProcessor, error) {
	if check.IfNil(arguments.HistoryStorer) {
		return nil, core.ErrNilStore
	}
	if check.IfNil(arguments.Marshalizer) {
		return nil, core.ErrNilMarshalizer
	}
	if check.IfNil(arguments.Hasher) {
		return nil, core.ErrNilHasher
	}
	if check.IfNil(arguments.HashEpochStorer) {
		return nil, core.ErrNilStore
	}

	hashEpochProc := newHashEpochProcessor(arguments.HashEpochStorer, arguments.Marshalizer)

	return &historyProcessor{
		selfShardID:   arguments.SelfShardID,
		historyStorer: arguments.HistoryStorer,
		marshalizer:   arguments.Marshalizer,
		hasher:        arguments.Hasher,
		hashEpochProc: hashEpochProc,
	}, nil
}

// PutTransactionsData will save in storage information about history transactions
func (hp *historyProcessor) PutTransactionsData(historyTxsData *HistoryTransactionsData) error {
	body, ok := historyTxsData.BodyHandler.(*block.Body)
	if !ok {
		return errors.New("cannot convert bodyHandler in body")
	}

	epoch := historyTxsData.HeaderHandler.GetEpoch()

	err := hp.hashEpochProc.SaveEpoch(historyTxsData.HeaderHash, epoch)
	if err != nil {
		log.Warn("epochHashProcessor:cannot save header hash", "error", err.Error())
		return err
	}

	for _, mb := range body.MiniBlocks {
		mbHash, err := core.CalculateHash(hp.marshalizer, hp.hasher, mb)
		if err != nil {
			continue
		}

		err = hp.hashEpochProc.SaveEpoch(mbHash, epoch)
		if err != nil {
			log.Warn("epochHashProcessor:cannot save miniblock hash", "error", err.Error())
			continue
		}

		var status core.TransactionStatus
		if mb.ReceiverShardID == hp.selfShardID {
			status = core.TxStatusExecuted
		} else {
			status = core.TxStatusPartiallyExecuted
		}

		historyTx := buildTransaction(historyTxsData.HeaderHandler, historyTxsData.HeaderHash, mbHash, mb.ReceiverShardID, mb.SenderShardID, status)

		historyTxBytes, err := hp.marshalizer.Marshal(historyTx)
		if err != nil {
			log.Debug("cannot marshal history transaction", "error", err.Error())
			continue
		}

		for _, txHash := range mb.TxHashes {
			err = hp.hashEpochProc.SaveEpoch(txHash, epoch)
			if err != nil {
				log.Trace("epochHashProcessor:cannot save transaction hash", "error", err.Error())
				continue
			}

			err = hp.historyStorer.Put(txHash, historyTxBytes)
			if err != nil {
				log.Debug("cannot save in storage history transaction",
					"hash", string(txHash),
					"error", err.Error())
				continue
			}
		}
	}

	return nil
}

func buildTransaction(
	headerHandler data.HeaderHandler,
	headerHash []byte,
	mbHash []byte,
	rcvShardID uint32,
	sndShardID uint32,
	status core.TransactionStatus,
) *HistoryTransaction {
	return &HistoryTransaction{
		HeaderHash:  headerHash,
		MbHash:      mbHash,
		Round:       headerHandler.GetRound(),
		HeaderNonce: headerHandler.GetNonce(),
		RcvShardID:  rcvShardID,
		SndShardID:  sndShardID,
		Status:      []byte(status),
	}
}

// GetTransaction will return a history transaction for the given hash from storage
func (hp *historyProcessor) GetTransaction(hash []byte) (*HistoryTransactionWithEpoch, error) {
	epoch, err := hp.hashEpochProc.GetEpoch(hash)
	if err != nil {
		return nil, err
	}

	historyTxBytes, err := hp.historyStorer.GetFromEpoch(hash, epoch)
	if err != nil {
		return nil, err
	}

	historyTx := &HistoryTransaction{}
	err = hp.marshalizer.Unmarshal(historyTx, historyTxBytes)
	if err != nil {
		return nil, err
	}

	return &HistoryTransactionWithEpoch{
		Epoch:              epoch,
		HistoryTransaction: historyTx,
	}, nil
}

// IsEnabled will always return true because this is implementation of a history processor
func (hp *historyProcessor) IsEnabled() bool {
	return true
}

// IsInterfaceNil returns true if there is no value under the interface
func (hp *historyProcessor) IsInterfaceNil() bool {
	return hp == nil
}