package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
)

// Remote Tempo networks (remote_rpc_url): nothing is generated or launched.
// Bootstrap funds the senders on the live chain instead of in a genesis, and
// signing starts from their on-chain nonces instead of 0.

const (
	// tempoNoncePrecompile holds the nonces of user nonce keys; key 0 is the
	// account's own nonce, which it refuses to report.
	tempoNoncePrecompile = "0x4E4F4E4345000000000000000000000000000000"

	// tempoGasPriceScale converts gas-price units (attodollars) to TIP-20 base
	// units (6 decimals).
	tempoGasPriceScale = 1_000_000_000_000

	tempoFundTimeout = 2 * time.Minute

	// Funding from account 0 packs this many transfers into one transaction,
	// the most the pool admits (MAX_AA_CALLS). All are payments, so the 30M
	// general-lane cap does not apply. A transfer that creates the
	// recipient's balance costs ~270k gas.
	tempoFundCallsPerTx = 32
	tempoFundGasPerCall = 300_000

	// publicTestMnemonic is what the devnet configs sign with. Its keys are
	// published, so on a public chain its accounts are everyone's.
	publicTestMnemonic = "test test test test test test test test test test test junk"
)

var (
	getNonceSelector  = []byte{0x89, 0x53, 0x58, 0x03} // getNonce(address,uint256)
	balanceOfSelector = []byte{0x70, 0xa0, 0x82, 0x31} // balanceOf(address)
)

func validateTempoRemote(spec messages.BenchmarkSpec) error {
	switch strings.Join(strings.Fields(spec.BaseMnemonic), " ") {
	case "":
		// Empty falls back to keygen's fixed seeds, which are just as public.
		return fmt.Errorf("remote_rpc_url needs a private base_mnemonic, and it is empty — is its environment variable set?")
	case publicTestMnemonic:
		return fmt.Errorf("remote_rpc_url needs a private base_mnemonic: the well-known test mnemonic's accounts are everyone's on a public chain")
	}
	if spec.TempoLegacyTxs {
		return fmt.Errorf("remote_rpc_url signs native transactions only: the legacy signer starts every account at nonce 0")
	}
	return nil
}

// bootstrapTempoRemote checks the endpoint serves the chain the transactions
// are signed for, then makes sure every sender can pay for the whole run.
func bootstrapTempoRemote(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	ids, err := readNumbers(ctx, spec, []bench.RPCCall{{Method: "eth_chainId", Params: []interface{}{}}})
	if err != nil {
		return fmt.Errorf("reach %s: %w", spec.RemoteRPCURL, err)
	}
	if ids[0].Cmp(big.NewInt(spec.EVMChainID)) != 0 {
		return fmt.Errorf("%s serves chain id %s but evm_chain_id is %d: every transaction would be signed for the wrong chain",
			spec.RemoteRPCURL, ids[0], spec.EVMChainID)
	}

	var senders []common.Address
	for _, n := range nodes {
		s, err := tempoSenders(spec, n)
		if err != nil {
			return err
		}
		senders = append(senders, s...)
	}
	if err := refuseStuckSenders(ctx, spec, senders); err != nil {
		return err
	}
	return fundTempoSenders(ctx, spec, senders)
}

// refuseStuckSenders stops a run while senders still have transactions in the
// node's pool from an earlier one: the new ones would queue behind them.
func refuseStuckSenders(ctx context.Context, spec messages.BenchmarkSpec, senders []common.Address) error {
	calls := make([]bench.RPCCall, 0, 2*len(senders))
	for _, addr := range senders {
		calls = append(calls,
			bench.RPCCall{Method: "eth_getTransactionCount", Params: []interface{}{addr.Hex(), "latest"}},
			bench.RPCCall{Method: "eth_getTransactionCount", Params: []interface{}{addr.Hex(), "pending"}})
	}
	counts, err := readNumbers(ctx, spec, calls)
	if err != nil {
		return fmt.Errorf("read nonces: %w", err)
	}
	var stuck []common.Address
	for i, addr := range senders {
		if counts[2*i+1].Cmp(counts[2*i]) > 0 {
			stuck = append(stuck, addr)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%d of %d senders still have transactions in the node's pool from an earlier run (first: %s); "+
			"clear them with `go run ./cmd/unstick -config <this config>`, then run again", len(stuck), len(senders), stuck[0].Hex())
	}
	return nil
}

// tempoSenders returns the addresses a node signs from, in the order
// generateTempoNativeTxs derives them.
func tempoSenders(spec messages.BenchmarkSpec, target messages.NodeTarget) ([]common.Address, error) {
	offset := target.GlobalSeq * spec.NumAccounts
	senders := make([]common.Address, spec.NumAccounts)
	for i := range senders {
		key, err := keygen.DeterministicKey(0, offset+i+1, spec.BaseMnemonic)
		if err != nil {
			return nil, fmt.Errorf("derive account %d: %w", offset+i+1, err)
		}
		senders[i] = crypto.PubkeyToAddress(key.PublicKey)
	}
	return senders, nil
}

// tempoFeeReserve is the most one account can spend in the run: every
// transaction at its gas limit and signed max fee, plus the unit it transfers.
// The pool admits a transaction only if its sender covers that maximum.
func tempoFeeReserve(spec messages.BenchmarkSpec) *big.Int {
	perTx := new(big.Int).SetUint64(spec.ERC20TransferGas)
	perTx.Mul(perTx, big.NewInt(spec.GasPriceWei))
	perTx.Div(perTx, big.NewInt(tempoGasPriceScale))
	perTx.Add(perTx, big.NewInt(1))
	return perTx.Mul(perTx, big.NewInt(int64(spec.NumTxs)))
}

// fundTempoSenders tops up every sender below the fee reserve, from account 0
// or the endpoint's faucet. With neither set it only reports them.
func fundTempoSenders(ctx context.Context, spec messages.BenchmarkSpec, senders []common.Address) error {
	need := tempoFeeReserve(spec)
	short, err := tempoShortOfFees(ctx, spec, senders, need)
	if err != nil {
		return err
	}
	if len(short) == 0 {
		fmt.Printf("[tempo-remote] all %d senders hold the %s fee-token units the run may spend\n", len(senders), need)
		return nil
	}
	switch {
	case spec.TempoFaucet:
		fmt.Printf("[tempo-remote] funding %d of %d senders from the faucet\n", len(short), len(senders))
		for _, addr := range short {
			if err := bench.JSONRPCCall(ctx, remoteClient, spec.RemoteRPCURL, "tempo_fundAddress", []string{addr.Hex()}, nil); err != nil {
				return fmt.Errorf("tempo_fundAddress %s: %w", addr.Hex(), err)
			}
		}
	case spec.TempoFundFromIndex0:
		if err := fundFromIndex0(ctx, spec, short, need); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%d of %d senders hold less than the %s fee-token units the run may spend (first: %s); "+
			"fund them, set tempo_fund_from_index0: true, or tempo_faucet: true on a testnet",
			len(short), len(senders), need, short[0].Hex())
	}

	// Funding answers with transaction hashes; balances follow once mined.
	deadline := time.Now().Add(tempoFundTimeout)
	for {
		if short, err = tempoShortOfFees(ctx, spec, short, need); err != nil {
			return err
		}
		if len(short) == 0 {
			fmt.Printf("[tempo-remote] funded\n")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d senders still hold less than %s fee-token units %s after funding (first: %s)",
				len(short), need, tempoFundTimeout, short[0].Hex())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// fundFromIndex0 sends each short sender the full reserve from account 0 of
// base_mnemonic, which never sends during a run.
func fundFromIndex0(ctx context.Context, spec messages.BenchmarkSpec, short []common.Address, need *big.Int) error {
	key, err := keygen.DeterministicKey(0, 0, spec.BaseMnemonic)
	if err != nil {
		return fmt.Errorf("derive account 0: %w", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	vals, err := readNumbers(ctx, spec, []bench.RPCCall{
		ethCall(tempoDefaultFeeToken, balanceOfSelector, from.Bytes()),
		{Method: "eth_getTransactionCount", Params: []interface{}{from.Hex(), "pending"}},
		{Method: "eth_gasPrice", Params: []interface{}{}},
	})
	if err != nil {
		return fmt.Errorf("read account 0: %w", err)
	}
	balance, nonce := vals[0], vals[1].Uint64()
	// Twice the current price, so the funding lands even if the fee moves.
	maxFee := max(2*vals[2].Uint64(), uint64(spec.GasPriceWei))

	cost := new(big.Int).Mul(need, big.NewInt(int64(len(short))))
	cost.Add(cost, new(big.Int).SetUint64(uint64(len(short))*tempoFundGasPerCall*maxFee/tempoGasPriceScale))
	if balance.Cmp(cost) < 0 {
		return fmt.Errorf("account 0 (%s) holds %s fee-token units, but funding %d senders needs %s",
			from.Hex(), balance, len(short), cost)
	}

	var raws []string
	for group := range slices.Chunk(short, tempoFundCallsPerTx) {
		calls := make([]tempotx.Call, len(group))
		for i, to := range group {
			calls[i] = tempotx.Call{To: tempotx.FeeToken, Data: tempotx.Transfer(to, need.Uint64())}
		}
		raw, err := (&tempotx.Tx{
			ChainID:              uint64(spec.EVMChainID),
			MaxPriorityFeePerGas: min(uint64(spec.TempoMaxPriorityFeePerGas), maxFee),
			MaxFeePerGas:         maxFee,
			GasLimit:             uint64(len(calls)) * tempoFundGasPerCall,
			Nonce:                nonce,
			FeeToken:             tempotx.FeeToken,
			Calls:                calls,
		}).SignedRaw(key)
		if err != nil {
			return fmt.Errorf("sign funding tx: %w", err)
		}
		raws = append(raws, raw)
		nonce++
	}

	fmt.Printf("[tempo-remote] funding %d senders from account 0 (%s) in %d transactions\n", len(short), from.Hex(), len(raws))
	// One worker, so the transactions arrive in nonce order.
	stats := bench.BroadcastRawTxs(ctx, remoteClient, spec.RemoteRPCURL, raws,
		bench.BroadcastOptions{BatchSize: spec.BroadcastBatchSize, Watermark: -1})
	if stats.Rejected+stats.Failed > 0 {
		return fmt.Errorf("funding from account 0: %d of %d transactions refused %v, %d unanswered",
			stats.Rejected, len(raws), stats.RejectReasons, stats.Failed)
	}
	return nil
}

// tempoShortOfFees returns the accounts whose fee-token balance is below need.
func tempoShortOfFees(ctx context.Context, spec messages.BenchmarkSpec, accounts []common.Address, need *big.Int) ([]common.Address, error) {
	calls := make([]bench.RPCCall, len(accounts))
	for i, addr := range accounts {
		calls[i] = ethCall(tempoDefaultFeeToken, balanceOfSelector, addr.Bytes())
	}
	balances, err := readNumbers(ctx, spec, calls)
	if err != nil {
		return nil, fmt.Errorf("read fee-token balances: %w", err)
	}
	var short []common.Address
	for i, b := range balances {
		if b.Cmp(need) < 0 {
			short = append(short, accounts[i])
		}
	}
	return short, nil
}

// tempoRemoteNonces reads the next nonce of every lane each of the node's
// senders will sign on: key 0 from the account, user keys from the precompile.
func tempoRemoteNonces(ctx context.Context, spec messages.BenchmarkSpec, target messages.NodeTarget) (map[common.Address][]uint64, error) {
	senders, err := tempoSenders(spec, target)
	if err != nil {
		return nil, err
	}
	lanes := tempoNonceLanes(spec)
	calls := make([]bench.RPCCall, 0, len(senders)*lanes)
	for _, addr := range senders {
		for lane := 0; lane < lanes; lane++ {
			key := uint64(spec.TempoNonceKey) + uint64(lane)
			if key == 0 {
				// "pending" also counts what an earlier run left in the pool.
				calls = append(calls, bench.RPCCall{Method: "eth_getTransactionCount", Params: []interface{}{addr.Hex(), "pending"}})
			} else {
				calls = append(calls, ethCall(tempoNoncePrecompile, getNonceSelector, addr.Bytes(), new(big.Int).SetUint64(key).Bytes()))
			}
		}
	}
	values, err := readNumbers(ctx, spec, calls)
	if err != nil {
		return nil, fmt.Errorf("read nonces: %w", err)
	}
	nonces := make(map[common.Address][]uint64, len(senders))
	for i, addr := range senders {
		for _, v := range values[i*lanes : (i+1)*lanes] {
			nonces[addr] = append(nonces[addr], v.Uint64())
		}
	}
	return nonces, nil
}

// remoteClient serves the few reads and faucet calls made before the load.
var remoteClient = &http.Client{Timeout: 10 * time.Second}

// ethCall is an eth_call of selector on `to` with 32-byte word arguments.
func ethCall(to string, selector []byte, words ...[]byte) bench.RPCCall {
	data := append([]byte{}, selector...)
	for _, w := range words {
		data = append(data, common.LeftPadBytes(w, 32)...)
	}
	return bench.RPCCall{Method: "eth_call", Params: []interface{}{
		map[string]string{"to": to, "data": hexutil.Encode(data)}, "latest",
	}}
}

// readNumbers runs calls whose results are hex numbers — quantities, or
// uint256 words from eth_call — batched to the endpoint's cap.
func readNumbers(ctx context.Context, spec messages.BenchmarkSpec, calls []bench.RPCCall) ([]*big.Int, error) {
	results, err := bench.JSONRPCBatch(ctx, remoteClient, spec.RemoteRPCURL, calls, spec.BroadcastBatchSize)
	if err != nil {
		return nil, err
	}
	values := make([]*big.Int, len(results))
	for i, raw := range results {
		var s string
		_ = json.Unmarshal(raw, &s)
		v, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
		if !ok {
			// eth_call answers a bare "0x" when the target has no code.
			return nil, fmt.Errorf("%s: not a number: %s", calls[i].Method, raw)
		}
		values[i] = v
	}
	return values, nil
}
