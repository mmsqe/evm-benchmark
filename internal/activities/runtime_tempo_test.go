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
		BaseMnemonic:     "test test test test test test test test test test test junk",
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
	genesis := filepath.Join(spec.DataDir, "devnet", "genesis.json")
	if _, err := os.Stat(genesis); err != nil {
		t.Fatalf("genesis not generated: %v", err)
	}
	for _, node := range nodes {
		for _, name := range []string{"run.sh", "signing.key", "enode.key"} {
			path := filepath.Join(tempoNodeHome(spec, node.GlobalSeq), name)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("node %d is missing %s: %v", node.GlobalSeq, name, err)
			}
		}
	}

}

// TestTempoRefusesOccupiedPort guards against silently benchmarking a node
// left over from a previous run, which serves its own stale genesis.
func TestTempoRefusesOccupiedPort(t *testing.T) {
	port := busyPort(t)
	spec := messages.BenchmarkSpec{
		ChainFamily:   FamilyTempo,
		DataDir:       t.TempDir(),
		TempoBasePort: port - tempoHTTPPortOffset,
	}
	nodes := []messages.NodeTarget{{GlobalSeq: 0}}
	err := tempoRuntime{}.Bootstrap(context.Background(), spec, nodes)
	if err == nil {
		t.Fatal("expected bootstrap to refuse an occupied RPC port")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestTempoRejectsNativeTransfers guards the step-1 finding: Tempo rejects
// native value transfers, so the generator must not be pointed at it with the
// simple-transfer shape.
func TestTempoRejectsNativeTransfers(t *testing.T) {
	spec := messages.BenchmarkSpec{TxType: messages.SimpleTransferTx}
	if err := (tempoRuntime{}).EnrichSpec(&spec); err == nil {
		t.Fatal("expected simple-transfer to be rejected on Tempo")
	}
}

// TestTempoRejectsInsufficientGas guards the ~271k intrinsic gas floor.
func TestTempoRejectsInsufficientGas(t *testing.T) {
	spec := messages.BenchmarkSpec{TxType: messages.ERC20TransferTx, ERC20TransferGas: 100000}
	if err := (tempoRuntime{}).EnrichSpec(&spec); err == nil {
		t.Fatal("expected sub-floor gas to be rejected on Tempo")
	}
}

// tempoNativeSpec is a minimal spec that drives the in-process native generator.
func tempoNativeSpec() messages.BenchmarkSpec {
	return messages.BenchmarkSpec{
		ChainFamily:               FamilyTempo,
		NumAccounts:               2,
		NumTxs:                    3,
		EVMChainID:                1337,
		BaseMnemonic:              "test test test test test test test test test test test junk",
		ERC20TransferGas:          300000,
		GasPriceWei:               40000000000,
		TempoMaxPriorityFeePerGas: 1000000000,
		ERC20ContractAddress:      tempoDefaultFeeToken,
	}
}

// TestTempoProduceTxsNative pins the in-process native generator: it writes a
// JSON array of exactly accounts*txs native (0x76) transactions.
func TestTempoProduceTxsNative(t *testing.T) {
	dir := t.TempDir()
	spec := tempoNativeSpec()
	txPath := filepath.Join(dir, "txs.json")

	count, err := (tempoRuntime{}).ProduceTxs(context.Background(), spec, messages.NodeTarget{GlobalSeq: 0}, txPath)
	if err != nil {
		t.Fatalf("ProduceTxs: %v", err)
	}
	if count != 6 {
		t.Fatalf("count = %d, want 6", count)
	}
	var raws []string
	if err := readJSON(txPath, &raws); err != nil {
		t.Fatalf("read txs: %v", err)
	}
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
// and silently halve the load. The output must match a tx built directly from
// the expected account key.
func TestTempoProduceTxsAccountOffset(t *testing.T) {
	dir := t.TempDir()
	spec := tempoNativeSpec()
	spec.NumAccounts = 1
	spec.NumTxs = 1
	txPath := filepath.Join(dir, "txs.json")

	if _, err := (tempoRuntime{}).ProduceTxs(context.Background(), spec, messages.NodeTarget{GlobalSeq: 2}, txPath); err != nil {
		t.Fatalf("ProduceTxs: %v", err)
	}
	var raws []string
	if err := readJSON(txPath, &raws); err != nil {
		t.Fatalf("read txs: %v", err)
	}

	// Node 2 with 1 account uses index 2*1+1 = 3.
	key, err := keygen.DeterministicKey(0, 3, spec.BaseMnemonic)
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	self := crypto.PubkeyToAddress(key.PublicKey)
	token := common.HexToAddress(tempoDefaultFeeToken)
	want, err := (&tempotx.Tx{
		ChainID: 1337, MaxPriorityFeePerGas: 1000000000, MaxFeePerGas: 40000000000,
		GasLimit: 300000, FeeToken: token,
		Calls: []tempotx.Call{{To: token, Data: tempotx.Transfer(self, 1)}},
	}).SignedRaw(key)
	if err != nil {
		t.Fatalf("build expected tx: %v", err)
	}
	if len(raws) != 1 || raws[0] != want {
		t.Errorf("node 2 first tx does not match a tx signed by account index 3: %v", raws)
	}
}

// TestTempoProduceTxsForwardsShape pins the workload-shape plumbing: a run
// labelled "hot" that silently generated the uncontended shape would report a
// contention number that never measured contention. self self-transfers; hot
// targets a shared recipient, so their signed bytes must differ.
func TestTempoProduceTxsForwardsShape(t *testing.T) {
	gen := func(shape string) string {
		dir := t.TempDir()
		spec := tempoNativeSpec()
		spec.NumAccounts = 1
		spec.NumTxs = 1
		spec.TempoTxShape = shape
		txPath := filepath.Join(dir, "txs.json")
		if _, err := (tempoRuntime{}).ProduceTxs(context.Background(), spec, messages.NodeTarget{}, txPath); err != nil {
			t.Fatalf("ProduceTxs %s: %v", shape, err)
		}
		var raws []string
		if err := readJSON(txPath, &raws); err != nil {
			t.Fatalf("read txs: %v", err)
		}
		if len(raws) != 1 {
			t.Fatalf("shape %s: wrote %d txs, want 1", shape, len(raws))
		}
		return raws[0]
	}
	if gen("self") == gen("hot") {
		t.Error("hot and self must target different recipients, so their txs must differ")
	}
}

// TestGenerateTxsUsesProducerAndFallsBack pins the txProducer wiring in both
// directions. If native delegation broke, Tempo runs would silently fall back
// to legacy transactions; if the fallback broke, every cosmos run would fail.
func TestGenerateTxsUsesProducerAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	base := messages.BenchmarkSpec{
		ChainFamily: FamilyTempo, DataDir: dir, NumAccounts: 2, NumTxs: 3,
		EVMChainID: 1337, BaseMnemonic: "test test test test test test test test test test test junk",
		ERC20TransferGas: 300000, GasPriceWei: 40000000000, TxType: messages.ERC20TransferTx,
	}

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
