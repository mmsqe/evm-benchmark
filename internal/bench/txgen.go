package bench

import (
	"fmt"
	"math/big"
	"runtime"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

const (
	SigningHeadroomReserved = 2
	SigningWorkerCap        = 8
)

func SigningWorkerCount(totalAccounts int) int {
	if totalAccounts <= 0 {
		return 0
	}
	return min(totalAccounts, min(SigningWorkerCap, max(1, runtime.GOMAXPROCS(0)-SigningHeadroomReserved)))
}

func GenerateSignedTxs(spec messages.BenchmarkSpec, globalSeq int) ([]string, error) {
	return GenerateSignedTxsWithProgress(spec, globalSeq, nil)
}

func GenerateSignedTxsWithProgress(spec messages.BenchmarkSpec, globalSeq int, onAccountDone func(done, total int)) ([]string, error) {
	result := make([]string, 0, spec.NumAccounts*spec.NumTxs)
	_, err := generateSignedTxs(spec, globalSeq, onAccountDone, func(raw string) error {
		result = append(result, raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func GenerateSignedTxsStream(spec messages.BenchmarkSpec, globalSeq int, onAccountDone func(done, total int), onTx func(raw string) error) (int, error) {
	return generateSignedTxs(spec, globalSeq, onAccountDone, onTx)
}

type signedAccountResult struct {
	index int
	txs   []string
	err   error
}

func generateSignedTxs(spec messages.BenchmarkSpec, globalSeq int, onAccountDone func(done, total int), onTx func(raw string) error) (int, error) {
	chainID := big.NewInt(spec.EVMChainID)
	gasPrice := big.NewInt(spec.GasPriceWei)
	totalAccounts := spec.NumAccounts
	if totalAccounts <= 0 {
		return 0, nil
	}

	workerCount := SigningWorkerCount(totalAccounts)
	jobs := make(chan int, totalAccounts)
	results := make(chan signedAccountResult, workerCount)

	for worker := 0; worker < workerCount; worker++ {
		go func() {
			for accountIndex := range jobs {
				txs, err := signAccountTxs(spec, globalSeq, accountIndex, chainID, gasPrice)
				results <- signedAccountResult{index: accountIndex, txs: txs, err: err}
			}
		}()
	}

	for accountIndex := 0; accountIndex < totalAccounts; accountIndex++ {
		jobs <- accountIndex
	}
	close(jobs)

	txCount := 0
	nextAccount := 0
	completed := 0
	pending := make(map[int][]string, workerCount)
	var firstErr error

	for completed < totalAccounts {
		result := <-results
		completed++
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		if firstErr != nil {
			continue
		}
		pending[result.index] = result.txs

		for {
			txs, ok := pending[nextAccount]
			if !ok {
				break
			}
			delete(pending, nextAccount)

			for _, raw := range txs {
				if onTx != nil {
					if err := onTx(raw); err != nil {
						firstErr = err
						break
					}
				}
				txCount++
			}

			if firstErr != nil {
				break
			}
			if onAccountDone != nil {
				onAccountDone(nextAccount+1, totalAccounts)
			}
			nextAccount++
		}
	}

	if firstErr != nil {
		return 0, firstErr
	}

	return txCount, nil
}

func signAccountTxs(spec messages.BenchmarkSpec, globalSeq, accountIndex int, chainID, gasPrice *big.Int) ([]string, error) {
	key, err := keygen.DeterministicKey(globalSeq, accountIndex+1, spec.BaseMnemonic)
	if err != nil {
		return nil, err
	}

	from := crypto.PubkeyToAddress(key.PublicKey)
	txs := make([]string, 0, spec.NumTxs)
	signer := types.NewLondonSigner(chainID)
	for nonce := 0; nonce < spec.NumTxs; nonce++ {
		tx, err := makeTx(spec, from, uint64(nonce), gasPrice)
		if err != nil {
			return nil, err
		}

		signed, err := types.SignTx(tx, signer, key)
		if err != nil {
			return nil, fmt.Errorf("sign tx: %w", err)
		}

		raw, err := signed.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("marshal tx: %w", err)
		}
		txs = append(txs, "0x"+common.Bytes2Hex(raw))
	}

	return txs, nil
}

func makeTx(spec messages.BenchmarkSpec, from common.Address, nonce uint64, gasPrice *big.Int) (*types.Transaction, error) {
	switch spec.TxType {
	case messages.SimpleTransferTx:
		return types.NewTransaction(nonce, from, big.NewInt(1), spec.SimpleTransferGas, gasPrice, nil), nil
	case messages.ERC20TransferTx:
		contract := common.HexToAddress(spec.ERC20ContractAddress)
		data := buildERC20TransferData(from, big.NewInt(1))
		return types.NewTransaction(nonce, contract, big.NewInt(0), spec.ERC20TransferGas, gasPrice, data), nil
	default:
		return nil, fmt.Errorf("unsupported tx type: %s", spec.TxType)
	}
}

func buildERC20TransferData(to common.Address, amount *big.Int) []byte {
	methodID := []byte{0xa9, 0x05, 0x9c, 0xbb}
	toBytes := common.LeftPadBytes(to.Bytes(), 32)
	amountBytes := common.LeftPadBytes(amount.Bytes(), 32)
	data := make([]byte, 0, len(methodID)+64)
	data = append(data, methodID...)
	data = append(data, toBytes...)
	data = append(data, amountBytes...)
	return data
}
