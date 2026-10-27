package activities

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
	"go.temporal.io/sdk/testsuite"
)

// TestTempoBootstrap exercises the real devnet-generation path (which shells
// out to `tempo-xtask generate-localnet`). It is skipped unless the toolchain
// is available:
//
//	TEMPO_BIN=.../tempo TEMPO_XTASK_BIN=.../tempo-xtask \
//	  go test ./internal/activities -run TempoBootstrap
func TestTempoBootstrap(t *testing.T) {
	tempoBin := os.Getenv("TEMPO_BIN")
	xtaskBin := os.Getenv("TEMPO_XTASK_BIN")
	if tempoBin == "" || xtaskBin == "" {
		t.Skip("set TEMPO_BIN and TEMPO_XTASK_BIN to run")
	}

	spec := messages.BenchmarkSpec{
		ChainFamily:      FamilyTempo,
		DataDir:          t.TempDir(),
		EVMChainID:       1337,
		NumAccounts:      8,
		BaseMnemonic:     publicTestMnemonic,
		Validators:       2,
		TempoBin:         tempoBin,
		TempoXtaskBin:    xtaskBin,
		TempoEpochLength: 100,
		TxType:           messages.ERC20TransferTx,
		ERC20TransferGas: tempoMinTxGas,
	}

	runtime := tempoRuntime{}
	if err := runtime.EnrichSpec(&spec); err != nil {
		t.Fatalf("EnrichSpec: %v", err)
	}
	if spec.ERC20ContractAddress != tempoDefaultFeeToken {
		t.Fatalf("expected the fee token to be defaulted, got %q", spec.ERC20ContractAddress)
	}

	nodes := []messages.NodeTarget{
		{GlobalSeq: 0, Group: "validators", GroupSeq: 0},
		{GlobalSeq: 1, Group: "validators", GroupSeq: 1},
	}
	if err := runtime.Bootstrap(context.Background(), spec, nodes); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// The generated network must be complete enough to launch: genesis plus a
	// per-node home with keys and a launcher.
	if _, err := os.Stat(filepath.Join(devnetDir(spec), "genesis.json")); err != nil {
		t.Fatalf("genesis not generated: %v", err)
	}
	for _, node := range nodes {
		for _, name := range []string{"run.sh", "signing.key", "enode.key"} {
			path := filepath.Join(devnetNodeHome(spec, node.GlobalSeq), name)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("node %d is missing %s: %v", node.GlobalSeq, name, err)
			}
		}
	}
}

// TestTempoEnrichSpecRejectsUnpayableLoad guards the loads Tempo would reject:
// native value transfers, and gas limits under the shape's floor (~271k, about
// twice that for fresh and approve), plus 22,100 for a new user nonce key.
func TestTempoEnrichSpecRejectsUnpayableLoad(t *testing.T) {
	erc20 := messages.ERC20TransferTx
	for _, tc := range []struct {
		name string
		spec messages.BenchmarkSpec
		want string // in the error; "" accepts
	}{
		{"native transfers", messages.BenchmarkSpec{TxType: messages.SimpleTransferTx}, "native value transfers"},
		{"sub-floor gas", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 100_000}, "below the floor"},
		{"key 0", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 280_000}, ""},
		{"nonce lanes", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 280_000, TempoNonceLanes: 2}, "on user nonce keys"},
		{"nonce key", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 280_000, TempoNonceKey: 5}, "on user nonce keys"},
		{"fresh", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 300_000, TempoTxShape: "fresh"}, "below the floor"},
		{"approve", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 300_000, TempoTxShape: "approve"}, "below the floor"},
		{"fresh with room", messages.BenchmarkSpec{TxType: erc20, ERC20TransferGas: 600_000, TempoTxShape: "fresh"}, ""},
	} {
		switch err := (tempoRuntime{}).EnrichSpec(&tc.spec); {
		case tc.want == "" && err != nil:
			t.Errorf("%s: rejected: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// tempoNativeSpec is a minimal spec that drives the in-process native generator.
func tempoNativeSpec() messages.BenchmarkSpec {
	return messages.BenchmarkSpec{
		ChainFamily:               FamilyTempo,
		NumAccounts:               2,
		NumTxs:                    3,
		EVMChainID:                1337,
		BaseMnemonic:              publicTestMnemonic,
		ERC20TransferGas:          300000,
		GasPriceWei:               40000000000,
		TempoMaxPriorityFeePerGas: 1000000000,
		ERC20ContractAddress:      tempoDefaultFeeToken,
	}
}

// produceTempoTxs runs the native generator for a node and returns what it
// wrote, which must be as many transactions as it reports.
func produceTempoTxs(t *testing.T, spec messages.BenchmarkSpec, globalSeq int) []string {
	t.Helper()
	txPath := filepath.Join(t.TempDir(), "txs.json")
	count, err := (tempoRuntime{}).ProduceTxs(context.Background(), spec, messages.NodeTarget{GlobalSeq: globalSeq}, txPath)
	if err != nil {
		t.Fatalf("ProduceTxs: %v", err)
	}
	var raws []string
	if err := readJSON(txPath, &raws); err != nil {
		t.Fatalf("read txs: %v", err)
	}
	if len(raws) != count {
		t.Fatalf("wrote %d txs, reported %d", len(raws), count)
	}
	return raws
}

// wantTempoTx signs the transaction the generator should emit from key index
// of spec's mnemonic, with spec's chain, fees and gas limit and the default fee
// token. Without calls it transfers one unit to itself.
func wantTempoTx(t *testing.T, spec messages.BenchmarkSpec, index int, nonceKey, nonce uint64, calls ...tempotx.Call) string {
	t.Helper()
	key, err := keygen.DeterministicKey(0, index, spec.BaseMnemonic)
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	token := common.HexToAddress(tempoDefaultFeeToken)
	if len(calls) == 0 {
		calls = []tempotx.Call{{To: token, Data: tempotx.Transfer(crypto.PubkeyToAddress(key.PublicKey), 1)}}
	}
	raw, err := (&tempotx.Tx{
		ChainID:              uint64(spec.EVMChainID),
		MaxPriorityFeePerGas: uint64(spec.TempoMaxPriorityFeePerGas),
		MaxFeePerGas:         uint64(spec.GasPriceWei),
		GasLimit:             spec.ERC20TransferGas,
		NonceKey:             nonceKey,
		Nonce:                nonce,
		FeeToken:             token,
		Calls:                calls,
	}).SignedRaw(key)
	if err != nil {
		t.Fatalf("build expected tx: %v", err)
	}
	return raw
}

// assertTxs reports every transaction that differs from the expected one.
func assertTxs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d txs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tx %d is not the expected account/lane/nonce/call", i)
		}
	}
}

// TestTempoProduceTxsNative pins the in-process native generator: it writes a
// JSON array of exactly accounts*txs native (0x76) transactions.
func TestTempoProduceTxsNative(t *testing.T) {
	raws := produceTempoTxs(t, tempoNativeSpec(), 0)
	if len(raws) != 6 {
		t.Fatalf("wrote %d txs, want 6", len(raws))
	}
	for i, r := range raws {
		if !strings.HasPrefix(r, "0x76") {
			t.Fatalf("tx %d is not a native envelope: %s", i, r)
		}
	}
}

// TestTempoProduceTxsAccountOffset pins the load-bearing offset: node N draws a
// disjoint slice starting at index N*accounts+1 of the single funded HD branch
// (global seq 0). Getting this wrong makes two nodes sign from the same accounts
// and silently halve the load.
func TestTempoProduceTxsAccountOffset(t *testing.T) {
	spec := tempoNativeSpec()
	spec.NumAccounts = 1
	spec.NumTxs = 1
	// Node 2 with 1 account uses index 2*1+1 = 3.
	assertTxs(t, produceTempoTxs(t, spec, 2), []string{wantTempoTx(t, spec, 3, 0, 0)})
}

// TestTempoProduceTxsForwardsShape pins the workload-shape plumbing: a run
// labelled "hot" that silently generated the uncontended shape would report a
// contention number that never measured contention. self self-transfers; hot
// targets a shared recipient, so their signed bytes must differ.
func TestTempoProduceTxsForwardsShape(t *testing.T) {
	gen := func(shape string) []string {
		spec := tempoNativeSpec()
		spec.NumAccounts = 1
		spec.NumTxs = 1
		spec.TempoTxShape = shape
		return produceTempoTxs(t, spec, 0)
	}
	if self, hot := gen("self"), gen("hot"); len(self) != 1 || len(hot) != 1 || self[0] == hot[0] {
		t.Error("hot and self must each write one tx, to different recipients")
	}
}

// TestTempoProduceTxsMultiLane pins tempo_nonce_lanes: an account's txs spread
// round-robin across lanes (nonce_key = base + lane), each lane sequential.
// This is the conflict workload — one owner issuing parallel-eligible txs.
func TestTempoProduceTxsMultiLane(t *testing.T) {
	spec := tempoNativeSpec()
	spec.NumAccounts = 1
	spec.NumTxs = 3
	spec.TempoNonceLanes = 2
	// tx0 -> lane0 (nk 0, nonce 0); tx1 -> lane1 (nk 1, nonce 0);
	// tx2 -> lane0 again (nk 0, nonce 1).
	assertTxs(t, produceTempoTxs(t, spec, 0), []string{
		wantTempoTx(t, spec, 1, 0, 0), wantTempoTx(t, spec, 1, 1, 0), wantTempoTx(t, spec, 1, 0, 1),
	})
}

// TestTempoProduceTxsSwapShape pins the order-book workload: tx 0/1 seed a bid
// and an ask wall on the DEX, then swaps alternate direction against the shared
// book. Verified end-to-end (all swaps mine with status 0x1) against a devnet.
func TestTempoProduceTxsSwapShape(t *testing.T) {
	spec := tempoNativeSpec()
	spec.NumAccounts = 1
	spec.NumTxs = 4
	spec.TempoTxShape = "swap"
	spec.ERC20TransferGas = 8000000
	dex := func(data []byte) tempotx.Call { return tempotx.Call{To: tempotx.StablecoinDEXAddress, Data: data} }
	assertTxs(t, produceTempoTxs(t, spec, 0), []string{
		wantTempoTx(t, spec, 1, 0, 0, dex(tempotx.PlaceFlip(tempoDexBase, tempoDexWallAmount, true, -100, 100))),
		wantTempoTx(t, spec, 1, 0, 1, dex(tempotx.PlaceFlip(tempoDexBase, tempoDexWallAmount, false, 100, -100))),
		wantTempoTx(t, spec, 1, 0, 2, dex(tempotx.SwapExactAmountIn(tempoDexBase, tempoDexQuote, tempoDexSwapAmount, 0))),
		wantTempoTx(t, spec, 1, 0, 3, dex(tempotx.SwapExactAmountIn(tempoDexQuote, tempoDexBase, tempoDexSwapAmount, 0))),
	})
}

// TestTempoProduceTxsSwapReplenishes pins the wall-replenish cycle: fresh bid/ask
// walls are re-seeded every tempoDexSwapsPerWall swaps so a long run cannot
// deplete the book (re-seeded walls verified to mine with status 0x1 on-chain).
func TestTempoProduceTxsSwapReplenishes(t *testing.T) {
	spec := tempoNativeSpec()
	spec.NumAccounts = 1
	spec.NumTxs = 2 + tempoDexSwapsPerWall + 4 // cross one cycle boundary
	spec.TempoTxShape = "swap"
	spec.ERC20TransferGas = 8000000
	raws := produceTempoTxs(t, spec, 0)
	const placeSel, swapSel = "922828f1", "f8856c0f" // placeFlip / swapExactAmountIn selectors
	wall := 2 + tempoDexSwapsPerWall
	for _, i := range []int{0, 1, wall, wall + 1} { // walls at each cycle start
		if !strings.Contains(raws[i], placeSel) {
			t.Errorf("tx %d should be a placeFlip wall", i)
		}
	}
	for _, i := range []int{2, wall - 1, wall + 2, wall + 3} { // swaps fill the rest
		if !strings.Contains(raws[i], swapSel) {
			t.Errorf("tx %d should be a swap", i)
		}
	}
}

// TestGenerateTxsUsesProducerAndFallsBack pins the txProducer wiring in both
// directions. If native delegation broke, Tempo runs would silently fall back
// to legacy transactions; if the fallback broke, every cosmos run would fail.
func TestGenerateTxsUsesProducerAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	base := tempoNativeSpec()
	base.DataDir = dir
	base.TxType = messages.ERC20TransferTx

	// GenerateTxs is a Temporal activity (it logs and heartbeats), so it needs
	// an activity context rather than context.Background().
	activityEnv := (&testsuite.WorkflowTestSuite{}).NewTestActivityEnvironment()
	act := &Activity{}
	activityEnv.RegisterActivity(act.GenerateTxs)

	run := func(spec messages.BenchmarkSpec) (int, error) {
		val, err := activityEnv.ExecuteActivity(act.GenerateTxs,
			messages.GenerateTxsRequest{Spec: spec, Target: messages.NodeTarget{GlobalSeq: 0}})
		if err != nil {
			return 0, err
		}
		var count int
		return count, val.Get(&count)
	}

	// Default (native): the runtime produces 0x76 envelopes itself.
	count, err := run(base)
	if err != nil {
		t.Fatalf("GenerateTxs native: %v", err)
	}
	if count != base.NumAccounts*base.NumTxs {
		t.Errorf("native count = %d, want %d", count, base.NumAccounts*base.NumTxs)
	}
	var raws []string
	if err := readJSON(filepath.Join(dir, "txs", "0.json"), &raws); err != nil {
		t.Fatalf("read txs: %v", err)
	}
	if len(raws) == 0 || !strings.HasPrefix(raws[0], "0x76") {
		t.Errorf("native path did not produce 0x76 txs: %v", raws)
	}

	// tempo_legacy_txs: must fall through to the built-in signer, which emits
	// legacy-typed transactions rather than 0x76.
	legacy := base
	legacy.TempoLegacyTxs = true
	count, err = run(legacy)
	if err != nil {
		t.Fatalf("GenerateTxs fallback: %v", err)
	}
	if count != base.NumAccounts*base.NumTxs {
		t.Errorf("fallback count = %d, want %d", count, base.NumAccounts*base.NumTxs)
	}
	if err := readJSON(filepath.Join(dir, "txs", "0.json"), &raws); err != nil {
		t.Fatalf("read txs: %v", err)
	}
	if len(raws) > 0 && strings.HasPrefix(raws[0], "0x76") {
		t.Error("fallback path produced native txs; the legacy signer should have run")
	}
}

// TestTempoDevnetFundsEveryNodeSlice guards the arithmetic that pairs
// Bootstrap's devnet `accounts` count with the generator's --account-offset:
// the last node's highest index must still be funded.
func TestTempoDevnetFundsEveryNodeSlice(t *testing.T) {
	for _, tc := range []struct{ validators, accounts int }{{1, 500}, {4, 500}, {2, 1}} {
		funded := tempoFundedAccounts(messages.BenchmarkSpec{
			NumAccounts: tc.accounts, Validators: tc.validators,
		})
		// generateTempoNativeTxs uses index = offset + i + 1, so the last node's
		// highest index is (validators-1)*accounts + accounts.
		highest := (tc.validators-1)*tc.accounts + tc.accounts
		if highest > funded-1 {
			t.Errorf("validators=%d accounts=%d: highest index %d exceeds funded 0..%d",
				tc.validators, tc.accounts, highest, funded-1)
		}
	}
}
