// Command checkfunding verifies that the accounts the load generator signs
// from can actually pay for and land transactions on a running chain.
//
// Run it before the first benchmark against a new chain, or after changing the
// mnemonic / account count:
//
//	go run ./cmd/checkfunding -rpc http://127.0.0.1:8004 -accounts 8 -chain-id 1337
//
// Genesis files are deliberately not inspected: on Tempo, account funding is
// materialised in precompile state and never appears in the genesis "alloc",
// and eth_getBalance reports the same synthetic native balance for *every*
// address (including unfunded ones), so neither is a usable funding signal.
// The trustworthy checks are the fee-token balance and a probe transaction.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
)

// ERC-20 selectors.
var (
	balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)
	transferSelector  = []byte{0xa9, 0x05, 0x9c, 0xbb} // transfer(address,uint256)
)

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8004", "EVM JSON-RPC endpoint")
	accounts := flag.Int("accounts", 8, "number of benchmark accounts to check")
	globalSeq := flag.Int("global-seq", 0, "node global sequence the keys are derived for")
	chainID := flag.Int64("chain-id", 1337, "EVM chain id used to sign the probe tx")
	mnemonic := flag.String("mnemonic", "test test test test test test test test test test test junk",
		"base mnemonic the benchmark signs with")
	feeToken := flag.String("fee-token", "0x20c0000000000000000000000000000000000000",
		"fee/gas token to check balanceOf against; empty to skip")
	probe := flag.Bool("probe", true, "send one transaction and wait for its receipt")
	probeGas := flag.Uint64("probe-gas", 400000, "gas limit for probe transactions (Tempo's intrinsic floor is ~271k)")
	flag.Parse()

	ctx := context.Background()
	client := &http.Client{Timeout: 10 * time.Second}

	height, err := bench.CurrentHeight(ctx, client, *rpcURL)
	if err != nil {
		fatal("chain is not reachable at %s: %v", *rpcURL, err)
	}
	fmt.Printf("chain height: %d\n", height)

	unfunded := 0
	for i := 0; i < *accounts; i++ {
		// Mirrors bench.signAccountTxs: index 0 is reserved for the validator key.
		index := i + 1
		key, err := keygen.DeterministicKey(*globalSeq, index, *mnemonic)
		if err != nil {
			fatal("derive account %d: %v", index, err)
		}
		addr := crypto.PubkeyToAddress(key.PublicKey)

		if *feeToken == "" {
			continue
		}
		balance, err := tokenBalance(ctx, client, *rpcURL, *feeToken, addr)
		if err != nil {
			fatal("query fee-token balance for %s: %v", addr.Hex(), err)
		}
		if balance.Sign() <= 0 {
			if unfunded < 5 { // keep the report short
				fmt.Printf("UNFUNDED index=%d %s\n", index, addr.Hex())
			}
			unfunded++
			continue
		}
		if i < 3 {
			fmt.Printf("funded   index=%d %s fee-token balance=%s\n", index, addr.Hex(), balance)
		}
	}
	if unfunded > 0 {
		fatal("%d/%d benchmark accounts hold no fee token", unfunded, *accounts)
	}
	if *feeToken != "" {
		fmt.Printf("all %d benchmark accounts hold the fee token\n", *accounts)
	}

	if !*probe {
		return
	}
	// Probe each tx shape the generator can emit: a chain that rejects one
	// (Tempo forbids native value transfers) must be benchmarked with another.
	shapes := []struct {
		name  string
		value *big.Int
		to    string
		gas   uint64
	}{
		{name: "native-transfer(value=1)", value: big.NewInt(1), gas: *probeGas},
		{name: "self-call(value=0)", value: big.NewInt(0), gas: *probeGas},
		{name: "fee-token transfer", value: big.NewInt(0), to: *feeToken, gas: *probeGas},
	}
	ok := 0
	for _, shape := range shapes {
		if err := probeTx(ctx, client, *rpcURL, *globalSeq, *chainID, *mnemonic, shape.name, shape.value, shape.to, shape.gas); err != nil {
			fmt.Printf("probe %-26s REJECTED: %v\n", shape.name, err)
			continue
		}
		fmt.Printf("probe %-26s OK\n", shape.name)
		ok++
	}
	if ok == 0 {
		fatal("no supported transaction shape: this chain cannot be driven by the current generator")
	}
	fmt.Println("\nOK: benchmark accounts are funded and can land transactions")
}

// probeTx signs one transaction of the given shape from the first benchmark
// account and waits for it to be included. This is the only end-to-end proof
// that generated transactions are valid for a chain: it exercises the fee
// model, the signer, the gas price and any value-transfer policy at once.
//
// An empty `to` sends to self; otherwise `to` is called with an ERC-20
// `transfer(self, 1)` payload.
func probeTx(ctx context.Context, client *http.Client, rpcURL string, globalSeq int, chainID int64, mnemonic, name string, value *big.Int, to string, gas uint64) error {
	key, err := keygen.DeterministicKey(globalSeq, 1, mnemonic)
	if err != nil {
		return err
	}
	from := crypto.PubkeyToAddress(key.PublicKey)

	gasPrice, err := bench.SuggestedGasPrice(ctx, client, rpcURL)
	if err != nil {
		return fmt.Errorf("suggest gas price: %w", err)
	}
	nonce, err := pendingNonce(ctx, client, rpcURL, from)
	if err != nil {
		return fmt.Errorf("pending nonce: %w", err)
	}

	target, data := from, []byte(nil)
	if to != "" {
		target = common.HexToAddress(to)
		data = append(append([]byte{}, transferSelector...), append(
			common.LeftPadBytes(from.Bytes(), 32),
			common.LeftPadBytes(big.NewInt(1).Bytes(), 32)...)...)
	}

	tx := types.NewTransaction(nonce, target, value, gas, big.NewInt(gasPrice), data)
	signed, err := types.SignTx(tx, types.NewLondonSigner(big.NewInt(chainID)), key)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	var sendResult string
	if err := bench.JSONRPCCall(ctx, client, rpcURL, "eth_sendRawTransaction",
		[]string{"0x" + common.Bytes2Hex(raw)}, &sendResult); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	fmt.Printf("probe: sent %s\n", sendResult)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var receipt struct {
			BlockNumber string `json:"blockNumber"`
			Status      string `json:"status"`
		}
		if err := bench.JSONRPCCall(ctx, client, rpcURL, "eth_getTransactionReceipt",
			[]string{sendResult}, &receipt); err == nil && receipt.BlockNumber != "" {
			if receipt.Status != "0x1" {
				return fmt.Errorf("transaction reverted (status %s)", receipt.Status)
			}
			fmt.Printf("probe: included in block %s\n", receipt.BlockNumber)
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("transaction %s was not included within 30s", sendResult)
}

func tokenBalance(ctx context.Context, client *http.Client, rpcURL, token string, holder common.Address) (*big.Int, error) {
	data := append(append([]byte{}, balanceOfSelector...), common.LeftPadBytes(holder.Bytes(), 32)...)
	call := map[string]string{"to": token, "data": "0x" + common.Bytes2Hex(data)}

	var result string
	if err := bench.JSONRPCCall(ctx, client, rpcURL, "eth_call", []interface{}{call, "latest"}, &result); err != nil {
		return nil, err
	}
	value, ok := new(big.Int).SetString(strings.TrimPrefix(result, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("unexpected balanceOf result %q", result)
	}
	return value, nil
}

func pendingNonce(ctx context.Context, client *http.Client, rpcURL string, addr common.Address) (uint64, error) {
	var hexNonce string
	if err := bench.JSONRPCCall(ctx, client, rpcURL, "eth_getTransactionCount",
		[]interface{}{addr.Hex(), "pending"}, &hexNonce); err != nil {
		return 0, err
	}
	value, ok := new(big.Int).SetString(strings.TrimPrefix(hexNonce, "0x"), 16)
	if !ok {
		return 0, fmt.Errorf("unexpected nonce %q", hexNonce)
	}
	return value.Uint64(), nil
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
