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

func generateSignedTxs(spec messages.BenchmarkSpec, globalSeq int, onAccountDone func(done, total int), onTx func(raw string) error) (int, error) {
	chainID := big.NewInt(spec.EVMChainID)
	gasPrice := big.NewInt(spec.GasPriceWei)
	txCount := 0
	for accountIndex := 0; accountIndex < spec.NumAccounts; accountIndex++ {
		key, err := keygen.DeterministicKey(globalSeq, accountIndex+1, spec.BaseMnemonic)
		if err != nil {
			return 0, err
		}

		from := crypto.PubkeyToAddress(key.PublicKey)
		for nonce := 0; nonce < spec.NumTxs; nonce++ {
			tx, err := makeTx(spec, from, uint64(nonce), gasPrice)
			if err != nil {
				return 0, err
			}

			signed, err := types.SignTx(tx, types.NewLondonSigner(chainID), key)
			if err != nil {
				return 0, fmt.Errorf("sign tx: %w", err)
			}

			raw, err := signed.MarshalBinary()
			if err != nil {
				return 0, fmt.Errorf("marshal tx: %w", err)
			}

			if onTx != nil {
				if err := onTx("0x" + common.Bytes2Hex(raw)); err != nil {
					return 0, err
				}
			}
			txCount++
		}
		if onAccountDone != nil {
			onAccountDone(accountIndex+1, spec.NumAccounts)
		}
	}

	return txCount, nil
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
