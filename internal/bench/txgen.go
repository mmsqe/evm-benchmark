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
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
)

const (
	// SigningHeadroomReserved is how many cores signing leaves to the rest of
	// the process.
	SigningHeadroomReserved = 2
	signingWorkerCap        = 8
)

// HotRecipient receives the hot_recipient_percent share of every account's
// transfers. No key controls it, so it only ever accumulates.
var HotRecipient = common.HexToAddress("0x1111111111111111111111111111111111111111")

func signingWorkerCount(totalAccounts int) int {
	return min(totalAccounts, signingWorkerCap, max(1, runtime.GOMAXPROCS(0)-SigningHeadroomReserved))
}

// GenerateSignedTxs returns every account's transactions, account by account.
func GenerateSignedTxs(spec messages.BenchmarkSpec, globalSeq int) ([]string, error) {
	result := make([]string, 0, spec.NumAccounts*spec.NumTxs)
	_, err := GenerateSignedTxsStream(spec, globalSeq, nil, func(raw string) error {
		result = append(result, raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type signedAccount struct {
	txs []string
	err error
}

// GenerateSignedTxsStream signs every account's transactions in parallel and
// hands them to onTx in account order; onAccountDone, if set, follows each
// account's last one.
func GenerateSignedTxsStream(spec messages.BenchmarkSpec, globalSeq int, onAccountDone func(done, total int), onTx func(raw string) error) (int, error) {
	total := spec.NumAccounts
	if total <= 0 {
		return 0, nil
	}
	chainID := big.NewInt(spec.EVMChainID)
	gasPrice := big.NewInt(spec.GasPriceWei)

	// Each account has its own result slot, so the loop below emits accounts
	// in order while the workers sign them in any.
	results := make([]chan signedAccount, total)
	for i := range results {
		results[i] = make(chan signedAccount, 1)
	}
	jobs := make(chan int, total)
	for i := range total {
		jobs <- i
	}
	close(jobs)
	for range signingWorkerCount(total) {
		go func() {
			for i := range jobs {
				txs, err := signAccountTxs(spec, globalSeq, i, chainID, gasPrice)
				results[i] <- signedAccount{txs, err}
			}
		}()
	}

	count := 0
	for i, ch := range results {
		r := <-ch
		if r.err != nil {
			return 0, r.err
		}
		for _, raw := range r.txs {
			if err := onTx(raw); err != nil {
				return 0, err
			}
		}
		count += len(r.txs)
		if onAccountDone != nil {
			onAccountDone(i+1, total)
		}
	}
	return count, nil
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
	to := from
	if sendsToHotRecipient(spec.HotRecipientPercent, nonce) {
		to = HotRecipient
	}
	switch spec.TxType {
	case messages.SimpleTransferTx:
		return types.NewTransaction(nonce, to, big.NewInt(1), spec.SimpleTransferGas, gasPrice, nil), nil
	case messages.ERC20TransferTx:
		contract := common.HexToAddress(spec.ERC20ContractAddress)
		return types.NewTransaction(nonce, contract, big.NewInt(0), spec.ERC20TransferGas, gasPrice, tempotx.Transfer(to, 1)), nil
	default:
		return nil, fmt.Errorf("unsupported tx type: %s", spec.TxType)
	}
}

// sendsToHotRecipient reports whether an account's transaction at nonce goes to
// HotRecipient. The hot nonces are spread evenly through the sequence, rather
// than the first percent of it, so every block of a run carries the same share
// of contended transactions.
func sendsToHotRecipient(percent int, nonce uint64) bool {
	p := uint64(percent)
	return (nonce+1)*p/100 > nonce*p/100
}
