package fullHistory

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/mock"
	"github.com/ElrondNetwork/elrond-go/data/block"
	"github.com/stretchr/testify/assert"
)

func createMockHistoryProcArgs() HistoryProcessorArguments {
	return HistoryProcessorArguments{
		Marshalizer:     &mock.MarshalizerMock{},
		Hasher:          &mock.HasherMock{},
		HistoryStorer:   &mock.StorerStub{},
		HashEpochStorer: &mock.StorerStub{},
		SelfShardID:     0,
	}
}

func TestNewHistoryProcessor_NilHistoryStorerShouldErr(t *testing.T) {
	t.Parallel()

	args := createMockHistoryProcArgs()
	args.HistoryStorer = nil

	proc, err := NewHistoryProcessor(args)
	assert.Nil(t, proc)
	assert.Equal(t, core.ErrNilStore, err)
}

func TestNewHistoryProcessor_NilHasherShouldErr(t *testing.T) {
	t.Parallel()

	args := createMockHistoryProcArgs()
	args.Hasher = nil

	proc, err := NewHistoryProcessor(args)
	assert.Nil(t, proc)
	assert.Equal(t, core.ErrNilHasher, err)
}

func TestNewHistoryProcessor_NilMarshalizerShouldErr(t *testing.T) {
	t.Parallel()

	args := createMockHistoryProcArgs()
	args.Marshalizer = nil

	proc, err := NewHistoryProcessor(args)
	assert.Nil(t, proc)
	assert.Equal(t, core.ErrNilMarshalizer, err)
}

func TestNewHistoryProcessor_NilHashEpochStorerShouldErr(t *testing.T) {
	t.Parallel()

	args := createMockHistoryProcArgs()
	args.HashEpochStorer = nil

	proc, err := NewHistoryProcessor(args)
	assert.Nil(t, proc)
	assert.Equal(t, core.ErrNilStore, err)
}

func TestNewHistoryProcessor(t *testing.T) {
	t.Parallel()

	args := createMockHistoryProcArgs()

	proc, err := NewHistoryProcessor(args)
	assert.NotNil(t, proc)
	assert.NoError(t, err)
	assert.True(t, proc.IsEnabled())
	assert.False(t, proc.IsInterfaceNil())
}

func TestHistoryProcessor_PutTransactionsData(t *testing.T) {
	t.Parallel()

	txHash := []byte("txHash")
	countCalledHashEpoch := 0
	args := createMockHistoryProcArgs()
	args.HashEpochStorer = &mock.StorerStub{
		PutCalled: func(key, data []byte) error {
			countCalledHashEpoch++
			return nil
		},
	}
	args.HistoryStorer = &mock.StorerStub{
		PutCalled: func(key, data []byte) error {
			assert.True(t, bytes.Equal(txHash, key))
			return nil
		},
	}

	proc, _ := NewHistoryProcessor(args)

	headerHash := []byte("headerHash")
	txsData := &HistoryTransactionsData{
		HeaderHash: headerHash,
		HeaderHandler: &block.Header{
			Epoch: 0,
		},
		BodyHandler: &block.Body{
			MiniBlocks: []*block.MiniBlock{
				{
					TxHashes:        [][]byte{txHash},
					SenderShardID:   0,
					ReceiverShardID: 1,
				},
			},
		},
	}

	err := proc.PutTransactionsData(txsData)
	assert.Nil(t, err)
}

func TestHistoryProcessor_GetTransaction(t *testing.T) {
	t.Parallel()

	epoch := uint32(10)
	args := createMockHistoryProcArgs()
	args.HashEpochStorer = &mock.StorerStub{
		GetCalled: func(key []byte) ([]byte, error) {
			hashEpochData := HashEpoch{
				Epoch: epoch,
			}

			hashEpochBytes, _ := json.Marshal(hashEpochData)
			return hashEpochBytes, nil
		},
	}

	round := uint64(1000)
	args.HistoryStorer = &mock.StorerStub{
		GetFromEpochCalled: func(key []byte, epoch uint32) ([]byte, error) {
			if epoch == epoch {
				historyTx := &HistoryTransaction{
					Round: round,
				}
				historyTxBytes, _ := json.Marshal(historyTx)
				return historyTxBytes, nil
			}
			return nil, nil
		},
	}

	proc, _ := NewHistoryProcessor(args)

	historyTx, err := proc.GetTransaction([]byte("txHash"))
	assert.Nil(t, err)
	assert.Equal(t, round, historyTx.Round)
	assert.Equal(t, epoch, historyTx.Epoch)
}