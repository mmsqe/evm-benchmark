package bench

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// TestSendsToHotRecipientSpreadsEvenly pins the hot share at every scale of a
// run: exactly percent of an account's transactions, spread through its nonces
// rather than bunched at the start, so each block of a run is equally
// contended.
func TestSendsToHotRecipientSpreadsEvenly(t *testing.T) {
	for _, percent := range []int{0, 1, 30, 50, 100} {
		hot := 0
		for nonce := uint64(0); nonce < 100; nonce++ {
			if sendsToHotRecipient(percent, nonce) {
				hot++
			}
		}
		if hot != percent {
			t.Errorf("percent %d: %d of 100 nonces hot", percent, hot)
		}
	}
	for start := uint64(0); start < 90; start += 10 {
		hot := 0
		for nonce := start; nonce < start+10; nonce++ {
			if sendsToHotRecipient(30, nonce) {
				hot++
			}
		}
		if hot != 3 {
			t.Errorf("nonces %d-%d: %d hot, want 3 of every 10", start, start+9, hot)
		}
	}
}

// TestHotRecipientTransfers checks the signed transactions themselves: a hot
// transfer names HotRecipient, every other one its own sender, for both shapes.
func TestHotRecipientTransfers(t *testing.T) {
	for _, txType := range []messages.TxType{messages.SimpleTransferTx, messages.ERC20TransferTx} {
		spec := messages.BenchmarkSpec{
			NumAccounts:          1,
			NumTxs:               10,
			EVMChainID:           1337,
			GasPriceWei:          1_000_000_000,
			SimpleTransferGas:    21_000,
			ERC20TransferGas:     60_000,
			ERC20ContractAddress: "0x2020202020202020202020202020202020202020",
			TxType:               txType,
			HotRecipientPercent:  50,
			BaseMnemonic:         "test test test test test test test test test test test junk",
		}
		raws, err := GenerateSignedTxs(spec, 0)
		if err != nil {
			t.Fatalf("%s: GenerateSignedTxs: %v", txType, err)
		}
		for _, raw := range raws {
			var tx types.Transaction
			if err := tx.UnmarshalBinary(hexutil.MustDecode(raw)); err != nil {
				t.Fatalf("%s: decode: %v", txType, err)
			}
			sender, err := types.Sender(types.NewLondonSigner(tx.ChainId()), &tx)
			if err != nil {
				t.Fatalf("%s: sender: %v", txType, err)
			}
			recipient := *tx.To()
			if txType == messages.ERC20TransferTx {
				recipient = common.BytesToAddress(tx.Data()[4:36])
			}
			want := sender
			if sendsToHotRecipient(spec.HotRecipientPercent, tx.Nonce()) {
				want = HotRecipient
			}
			if recipient != want {
				t.Errorf("%s nonce %d: recipient %s, want %s", txType, tx.Nonce(), recipient.Hex(), want.Hex())
			}
		}
	}
}
