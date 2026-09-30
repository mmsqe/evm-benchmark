package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
)

// A valid mnemonic other than the devnets' public one; remote mode refuses that.
const testPrivateMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// fakeTempo is a live Tempo chain as far as remote mode sees one: a chain id,
// fee-token balances, protocol and user-key nonces, and a faucet.
type fakeTempo struct {
	mu        sync.Mutex
	chainID   int64
	balances  map[common.Address]*big.Int
	nonces    map[common.Address]uint64 // protocol nonce (key 0)
	pooled    map[common.Address]uint64 // transactions the node holds past it
	keyNonces map[string]uint64         // "addr/key" for user keys
	grant     *big.Int
	funded    []common.Address
}

func newFakeTempo() *fakeTempo {
	return &fakeTempo{
		chainID:   42431,
		balances:  map[common.Address]*big.Int{},
		nonces:    map[common.Address]uint64{},
		pooled:    map[common.Address]uint64{},
		keyNonces: map[string]uint64{},
		grant:     big.NewInt(1_000_000_000_000),
	}
}

func word(v *big.Int) string { return "0x" + common.Bytes2Hex(common.LeftPadBytes(v.Bytes(), 32)) }

func (f *fakeTempo) handle(method string, params json.RawMessage) (interface{}, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p []json.RawMessage
	_ = json.Unmarshal(params, &p)
	switch method {
	case "eth_chainId":
		return fmt.Sprintf("0x%x", f.chainID), ""
	case "eth_getTransactionCount":
		var addr, tag string
		_ = json.Unmarshal(p[0], &addr)
		_ = json.Unmarshal(p[1], &tag)
		a := common.HexToAddress(addr)
		if tag == "pending" {
			return fmt.Sprintf("0x%x", f.nonces[a]+f.pooled[a]), ""
		}
		return fmt.Sprintf("0x%x", f.nonces[a]), ""
	case "eth_call":
		var call struct{ To, Data string }
		_ = json.Unmarshal(p[0], &call)
		data := common.FromHex(call.Data)
		addr := common.BytesToAddress(data[4:36])
		switch {
		case strings.EqualFold(call.To, tempoDefaultFeeToken):
			balance := f.balances[addr]
			if balance == nil {
				balance = new(big.Int)
			}
			return word(balance), ""
		case strings.EqualFold(call.To, tempoNoncePrecompile):
			key := new(big.Int).SetBytes(data[36:68])
			if key.Sign() == 0 {
				return nil, "protocol nonce not supported"
			}
			return word(new(big.Int).SetUint64(f.keyNonces[fmt.Sprintf("%s/%s", addr.Hex(), key)])), ""
		}
		return "0x", ""
	case "tempo_fundAddress":
		var addr string
		_ = json.Unmarshal(p[0], &addr)
		a := common.HexToAddress(addr)
		if f.balances[a] == nil {
			f.balances[a] = new(big.Int)
		}
		f.balances[a].Add(f.balances[a], f.grant)
		f.funded = append(f.funded, a)
		return []string{"0x01", "0x02", "0x03", "0x04"}, ""
	}
	return nil, "unexpected " + method
}

// serve exposes the chain over JSON-RPC, single and batched.
func (f *fakeTempo) serve(t *testing.T) *httptest.Server {
	t.Helper()
	answer := func(req map[string]json.RawMessage) map[string]interface{} {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		result, errMsg := f.handle(method, req["params"])
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": req["id"]}
		if errMsg != "" {
			resp["error"] = map[string]interface{}{"code": -32000, "message": errMsg}
		} else {
			resp["result"] = result
		}
		return resp
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			var reqs []map[string]json.RawMessage
			_ = json.Unmarshal(body, &reqs)
			out := make([]map[string]interface{}, len(reqs))
			for i, req := range reqs {
				out[i] = answer(req)
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		var req map[string]json.RawMessage
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(answer(req))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tempoRemoteSpec(url string) messages.BenchmarkSpec {
	spec := tempoNativeSpec()
	spec.RemoteRPCURL = url
	spec.BaseMnemonic = testPrivateMnemonic
	spec.EVMChainID = 42431
	spec.GasPriceWei = 3_000_000_000
	spec.Validators = 1
	spec.BroadcastBatchSize = 5
	return spec
}

func remoteSender(t *testing.T, index int) common.Address {
	t.Helper()
	key, err := keygen.DeterministicKey(0, index, testPrivateMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	return crypto.PubkeyToAddress(key.PublicKey)
}

// TestTempoRemoteRefusesPublicKeys: on a public chain the devnets' mnemonic —
// and keygen's fixed fallback seeds when none is set — are everyone's accounts.
func TestTempoRemoteRefusesPublicKeys(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*messages.BenchmarkSpec)
		want   string
	}{
		{"empty mnemonic", func(s *messages.BenchmarkSpec) { s.BaseMnemonic = "" }, "environment variable"},
		{"public mnemonic", func(s *messages.BenchmarkSpec) {
			s.BaseMnemonic = " test test test test test test test test test test test  junk"
		}, "well-known"},
		{"legacy signer", func(s *messages.BenchmarkSpec) { s.TempoLegacyTxs = true }, "nonce 0"},
	} {
		spec := tempoRemoteSpec("http://example.invalid")
		tc.mutate(&spec)
		if err := (tempoRuntime{}).Validate(spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	if err := (tempoRuntime{}).Validate(tempoRemoteSpec("http://example.invalid")); err != nil {
		t.Errorf("private mnemonic refused: %v", err)
	}
}

// TestTempoRemoteBootstrapFundsOnlyShortAccounts: the faucet is asked only for
// accounts that cannot cover the run, so a rerun spends nothing.
func TestTempoRemoteBootstrapFundsOnlyShortAccounts(t *testing.T) {
	chain := newFakeTempo()
	srv := chain.serve(t)
	spec := tempoRemoteSpec(srv.URL)
	spec.TempoFaucet = true
	rich := remoteSender(t, 1)
	poor := remoteSender(t, 2)
	chain.balances[rich] = tempoFeeReserve(spec)
	chain.balances[poor] = new(big.Int).Sub(tempoFeeReserve(spec), big.NewInt(1))

	if err := (tempoRuntime{}).Bootstrap(context.Background(), spec, []messages.NodeTarget{{GlobalSeq: 0}}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(chain.funded) != 1 || chain.funded[0] != poor {
		t.Errorf("faucet funded %v, want only %s", chain.funded, poor.Hex())
	}
	if err := (tempoRuntime{}).Bootstrap(context.Background(), spec, []messages.NodeTarget{{GlobalSeq: 0}}); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if len(chain.funded) != 1 {
		t.Errorf("rerun called the faucet again: %v", chain.funded)
	}
}

// TestTempoRemoteRefusesStuckSenders: a sender with transactions still in the
// node's pool would queue the new run behind them, so the run must not start.
func TestTempoRemoteRefusesStuckSenders(t *testing.T) {
	chain := newFakeTempo()
	srv := chain.serve(t)
	spec := tempoRemoteSpec(srv.URL)
	spec.TempoFaucet = true
	stuck := remoteSender(t, 2)
	chain.nonces[stuck] = 3387
	chain.pooled[stuck] = 221

	err := (tempoRuntime{}).Bootstrap(context.Background(), spec, []messages.NodeTarget{{GlobalSeq: 0}})
	if err == nil || !strings.Contains(err.Error(), "unstick") || !strings.Contains(err.Error(), stuck.Hex()) {
		t.Fatalf("err = %v, want the stuck sender named with the unstick fix", err)
	}
	if len(chain.funded) != 0 {
		t.Error("funded senders for a run that must not start")
	}
}

// TestTempoRemoteReserveMatchesChainFees pins the fee scale against a
// Moderato receipt: 276,318 gas at 1.6 gwei charged 443 fee-token units
// (442.1 rounded up), so gas-price units are 1e12 per token base unit.
func TestTempoRemoteReserveMatchesChainFees(t *testing.T) {
	spec := messages.BenchmarkSpec{NumTxs: 1, ERC20TransferGas: 276318, GasPriceWei: 1_600_000_000}
	if got := tempoFeeReserve(spec); got.Cmp(big.NewInt(442+1)) != 0 {
		t.Errorf("reserve = %s, want 443 (442 fee + the unit transferred)", got)
	}
}

func TestTempoRemoteBootstrapWithoutFaucetNamesTheFix(t *testing.T) {
	chain := newFakeTempo()
	srv := chain.serve(t)
	spec := tempoRemoteSpec(srv.URL)

	err := (tempoRuntime{}).Bootstrap(context.Background(), spec, []messages.NodeTarget{{GlobalSeq: 0}})
	if err == nil || !strings.Contains(err.Error(), "tempo_faucet") {
		t.Fatalf("err = %v, want unfunded accounts reported with the tempo_faucet fix", err)
	}
	if len(chain.funded) != 0 {
		t.Error("faucet called without tempo_faucet")
	}
}

func TestTempoRemoteBootstrapRejectsWrongChain(t *testing.T) {
	chain := newFakeTempo()
	chain.chainID = 4217
	srv := chain.serve(t)
	spec := tempoRemoteSpec(srv.URL)
	spec.TempoFaucet = true

	err := (tempoRuntime{}).Bootstrap(context.Background(), spec, []messages.NodeTarget{{GlobalSeq: 0}})
	if err == nil || !strings.Contains(err.Error(), "wrong chain") {
		t.Fatalf("err = %v, want a chain id mismatch", err)
	}
	if len(chain.funded) != 0 {
		t.Error("faucet called on the wrong chain")
	}
}

// TestTempoRemoteProduceTxsStartsFromChainNonces: accounts on a live chain
// have sent before, so each lane starts at its on-chain nonce — the account's
// own for key 0, the nonce precompile's for user keys — and accounts take
// turns so no sender's backlog runs ahead.
func TestTempoRemoteProduceTxsStartsFromChainNonces(t *testing.T) {
	chain := newFakeTempo()
	srv := chain.serve(t)
	spec := tempoRemoteSpec(srv.URL)
	spec.NumAccounts = 2
	spec.NumTxs = 3
	spec.TempoNonceLanes = 2
	a, b := remoteSender(t, 1), remoteSender(t, 2)
	chain.nonces[a] = 7
	chain.keyNonces[a.Hex()+"/1"] = 3

	txPath := filepath.Join(t.TempDir(), "txs.json")
	if _, err := (tempoRuntime{}).ProduceTxs(context.Background(), spec, messages.NodeTarget{GlobalSeq: 0}, txPath); err != nil {
		t.Fatalf("ProduceTxs: %v", err)
	}
	var raws []string
	if err := readJSON(txPath, &raws); err != nil {
		t.Fatal(err)
	}

	token := common.HexToAddress(tempoDefaultFeeToken)
	mk := func(index int, self common.Address, nonceKey, nonce uint64) string {
		key, _ := keygen.DeterministicKey(0, index, testPrivateMnemonic)
		raw, err := (&tempotx.Tx{
			ChainID: 42431, MaxPriorityFeePerGas: 1000000000, MaxFeePerGas: 3000000000,
			GasLimit: 300000, NonceKey: nonceKey, Nonce: nonce, FeeToken: token,
			Calls: []tempotx.Call{{To: token, Data: tempotx.Transfer(self, 1)}},
		}).SignedRaw(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	want := []string{
		mk(1, a, 0, 7), mk(2, b, 0, 0),
		mk(1, a, 1, 3), mk(2, b, 1, 0),
		mk(1, a, 0, 8), mk(2, b, 0, 1),
	}
	if len(raws) != len(want) {
		t.Fatalf("got %d txs, want %d", len(raws), len(want))
	}
	for i := range want {
		if raws[i] != want[i] {
			t.Errorf("tx %d is not the expected account/lane/nonce", i)
		}
	}
}

// TestTempoGasFloorCoversNewNonceKey: the first transaction on a user nonce
// key pays 22,100 gas more, so a limit that clears the key-0 floor can still
// be rejected once lanes or a nonce key are set.
func TestTempoGasFloorCoversNewNonceKey(t *testing.T) {
	spec := messages.BenchmarkSpec{TxType: messages.ERC20TransferTx, ERC20TransferGas: 280000}
	if err := (tempoRuntime{}).EnrichSpec(&spec); err != nil {
		t.Fatalf("key 0: %v", err)
	}
	for _, s := range []messages.BenchmarkSpec{
		{TxType: messages.ERC20TransferTx, ERC20TransferGas: 280000, TempoNonceLanes: 2},
		{TxType: messages.ERC20TransferTx, ERC20TransferGas: 280000, TempoNonceKey: 5},
	} {
		if err := (tempoRuntime{}).EnrichSpec(&s); err == nil || !strings.Contains(err.Error(), "nonce keys") {
			t.Errorf("lanes=%d key=%d: err = %v, want the nonce-key surcharge enforced", s.TempoNonceLanes, s.TempoNonceKey, err)
		}
	}
}

// TestGenerateLayoutRemote: a remote layout points every node at the endpoint
// and generates no devnet, so it needs neither tempo binary.
func TestGenerateLayoutRemote(t *testing.T) {
	chain := newFakeTempo()
	srv := chain.serve(t)
	spec := tempoRemoteSpec(srv.URL)
	spec.TempoFaucet = true
	spec.DataDir = t.TempDir()

	res, err := (&Activity{}).GenerateLayout(context.Background(), messages.GenerateLayoutRequest{Spec: spec})
	if err != nil {
		t.Fatalf("GenerateLayout: %v", err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].RPCURL != srv.URL {
		t.Fatalf("nodes = %+v, want one node on %s", res.Nodes, srv.URL)
	}
	if _, err := os.Stat(devnetDir(spec)); !os.IsNotExist(err) {
		t.Errorf("a devnet was generated for a remote run (stat err = %v)", err)
	}
	if len(chain.funded) != 2 {
		t.Errorf("faucet funded %d accounts, want both senders", len(chain.funded))
	}
}
