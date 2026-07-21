package activities

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmsqe/evm-benchmark/internal/messages"
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

// recordingStub writes a stub generator that records its argv to args.txt and
// writes the given tx JSON array to --out, so a test can both assert the flags
// and observe the produced count. Returns the stub path and the args file.
func recordingStub(t *testing.T, dir, txsJSON string, count int) (stub, argsFile string) {
	t.Helper()
	stub = filepath.Join(dir, "stub.sh")
	argsFile = filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"out=\"\"; while [ $# -gt 0 ]; do if [ \"$1\" = --out ]; then out=$2; fi; shift; done\n" +
		"printf '%s' '" + txsJSON + "' > \"$out\"\n" +
		fmt.Sprintf("printf '{\"txs\":%d}\\n'\n", count)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stub, argsFile
}

// TestTempoProduceTxsArguments pins the flags handed to the external
// generator. These are load-bearing: --global-seq must stay 0 because
// tempo-xtask funds only that HD branch, and --account-offset is what keeps
// two nodes from signing with the same accounts (which would collide on
// nonces and silently halve the load).
func TestTempoProduceTxsArguments(t *testing.T) {
	dir := t.TempDir()
	stub, argsFile := recordingStub(t, dir, `["0x76aa","0x76bb"]`, 2)

	spec := messages.BenchmarkSpec{
		ChainFamily:      FamilyTempo,
		TempoTxGenerator: stub,
		NumAccounts:      500,
		NumTxs:           100,
		EVMChainID:       1337,
		BaseMnemonic:     "test test test test test test test test test test test junk",
		ERC20TransferGas: 300000,
		GasPriceWei:      40000000000,
	}
	txPath := filepath.Join(dir, "txs.json")

	count, err := (tempoRuntime{}).ProduceTxs(
		context.Background(), spec, messages.NodeTarget{GlobalSeq: 2}, txPath)
	if err != nil {
		t.Fatalf("ProduceTxs: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	got := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			got[args[i]] = args[i+1]
		}
	}

	want := map[string]string{
		// Every node signs from the one branch tempo-xtask funds...
		"--global-seq": "0",
		// ...and node 2 takes the slice after nodes 0 and 1 (2 * 500).
		"--account-offset":  "1000",
		"--accounts":        "500",
		"--txs-per-account": "100",
		"--chain-id":        "1337",
		"--gas-limit":       "300000",
		"--max-fee-per-gas": "40000000000",
		"--out":             txPath,
	}
	for flag, expected := range want {
		if got[flag] != expected {
			t.Errorf("%s = %q, want %q", flag, got[flag], expected)
		}
	}
	// The fee token must default to the genesis TIP-20 rather than empty.
	if got["--token"] != tempoDefaultFeeToken {
		t.Errorf("--token = %q, want %q", got["--token"], tempoDefaultFeeToken)
	}
	// A shape is only forwarded when configured, so the generator keeps its own
	// default; batch size rides along with it.
	if _, ok := got["--tx-shape"]; ok {
		t.Error("--tx-shape must not be passed when unset")
	}
	// Gas is always paid in the genesis fee token, even if the transfer target
	// is customised, and each node gets a share of the host rather than all of it.
	if got["--fee-token"] != tempoDefaultFeeToken {
		t.Errorf("--fee-token = %q, want %q", got["--fee-token"], tempoDefaultFeeToken)
	}
	if got["--workers"] == "" || got["--workers"] == "0" {
		t.Errorf("--workers must be set to bound per-node signing parallelism, got %q", got["--workers"])
	}
}

// TestGenerateTxsUsesProducerAndFallsBack pins the txProducer wiring in both
// directions. If the delegation broke, Tempo runs would silently fall back to
// legacy transactions; if the fallback broke, every cosmos run would fail.
// TestTempoProduceTxsForwardsShape pins the workload-shape plumbing: a run
// labelled "hot" that silently generated the uncontended shape would report a
// contention number that never measured contention.
func TestTempoProduceTxsForwardsShape(t *testing.T) {
	dir := t.TempDir()
	stub, argsFile := recordingStub(t, dir, `["0x76aa"]`, 1)

	spec := messages.BenchmarkSpec{
		TempoTxGenerator: stub, NumAccounts: 1, NumTxs: 1, EVMChainID: 1337,
		BaseMnemonic: "test", ERC20TransferGas: 300000, GasPriceWei: 1,
		TempoTxShape: "hot", TempoBatchCalls: 8,
	}
	if _, err := (tempoRuntime{}).ProduceTxs(
		context.Background(), spec, messages.NodeTarget{}, filepath.Join(dir, "txs.json")); err != nil {
		t.Fatalf("ProduceTxs: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	got := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			got[args[i]] = args[i+1]
		}
	}
	if got["--tx-shape"] != "hot" {
		t.Errorf("--tx-shape = %q, want hot", got["--tx-shape"])
	}
	if got["--batch-calls"] != "8" {
		t.Errorf("--batch-calls = %q, want 8", got["--batch-calls"])
	}
}

func TestGenerateTxsUsesProducerAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.sh")
	script := "#!/bin/sh\nout=\"\"; while [ $# -gt 0 ]; do if [ \"$1\" = --out ]; then out=$2; fi; shift; done\n" +
		"printf '[\"0x76aa\",\"0x76bb\",\"0x76cc\"]' > \"$out\"\nprintf '{\"txs\":3}\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

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

	// Producer configured: its output must be used verbatim.
	withProducer := base
	withProducer.TempoTxGenerator = stub
	count, err := run(withProducer)
	if err != nil {
		t.Fatalf("GenerateTxs with producer: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3 (producer output)", count)
	}
	var raws []string
	if err := readJSON(filepath.Join(dir, "txs", "0.json"), &raws); err != nil {
		t.Fatalf("read txs: %v", err)
	}
	if len(raws) != 3 || !strings.HasPrefix(raws[0], "0x76") {
		t.Errorf("producer output was overwritten by the legacy signer: %v", raws)
	}

	// No producer: must fall through to the built-in signer, which emits
	// legacy-typed transactions rather than 0x76.
	count, err = run(base)
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
		t.Error("fallback path produced native txs; the producer should not have run")
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
		// gen_tempo_txs.py uses index = offset + i + 1, so the last node's
		// highest index is (validators-1)*accounts + accounts.
		highest := (tc.validators-1)*tc.accounts + tc.accounts
		if highest > funded-1 {
			t.Errorf("validators=%d accounts=%d: highest index %d exceeds funded 0..%d",
				tc.validators, tc.accounts, highest, funded-1)
		}
	}
}
