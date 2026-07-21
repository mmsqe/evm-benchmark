package activities

import (
	"net"
	"reflect"
	"testing"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// The cosmos bootstrap was moved behind ChainRuntime; these pin the behaviour
// that used to be inline in RunNode/GenerateLayout so the refactor cannot
// silently change how existing evmd/mantrachaind runs are launched.

func TestResolveRuntimeDefaultsToCosmos(t *testing.T) {
	for _, family := range []string{"", "cosmos"} {
		chain, err := resolveRuntime(messages.BenchmarkSpec{ChainFamily: family})
		if err != nil {
			t.Fatalf("family %q: %v", family, err)
		}
		if chain.Name() != FamilyCosmos {
			t.Errorf("family %q resolved to %s", family, chain.Name())
		}
	}
	if _, err := resolveRuntime(messages.BenchmarkSpec{ChainFamily: "bogus"}); err == nil {
		t.Error("expected an unknown chain family to be rejected")
	}
}

func TestCosmosLocalStartCommand(t *testing.T) {
	spec := messages.BenchmarkSpec{
		Binary:    "/bin/evmd",
		ChainID:   "evmd_262144-1",
		StartArgs: []string{"--log_level", "info"},
	}
	target := messages.NodeTarget{Home: "/data/validators/0"}

	argv, dir := cosmosRuntime{}.LocalStartCommand(spec, target)
	want := []string{"/bin/evmd", "start", "--home", "/data/validators/0", "--chain-id", "evmd_262144-1", "--log_level", "info"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if dir != "" {
		t.Errorf("cosmos nodes must keep the caller's working directory, got %q", dir)
	}

	// A chain id is optional and must simply be omitted when unset.
	spec.ChainID = ""
	argv, _ = cosmosRuntime{}.LocalStartCommand(spec, target)
	want = []string{"/bin/evmd", "start", "--home", "/data/validators/0", "--log_level", "info"}
	if !reflect.DeepEqual(argv, want) {
		t.Errorf("argv without chain id = %v, want %v", argv, want)
	}
}

func TestCosmosEVMRPCPort(t *testing.T) {
	spec := messages.BenchmarkSpec{EVMRPCPort: 8545}
	if got := (cosmosRuntime{}).EVMRPCPort(spec, 3); got != 8545 {
		t.Errorf("local mode must use the fixed port, got %d", got)
	}
	spec.RunnerType = "docker"
	if got := (cosmosRuntime{}).EVMRPCPort(spec, 3); got != 8548 {
		t.Errorf("docker mode must offset per node, got %d", got)
	}
}

func TestCosmosValidateRequiresBinary(t *testing.T) {
	if err := (cosmosRuntime{}).Validate(messages.BenchmarkSpec{StartNode: true}); err == nil {
		t.Error("expected a missing binary to be rejected when start_node=true")
	}
	if err := (cosmosRuntime{}).Validate(messages.BenchmarkSpec{StartNode: false}); err != nil {
		t.Errorf("binary is not needed when start_node=false: %v", err)
	}
}

func TestTempoDockerValidation(t *testing.T) {
	base := messages.BenchmarkSpec{
		RunnerType:       "docker",
		TempoDockerImage: "ghcr.io/tempoxyz/tempo:latest",
		Validators:       4,
	}
	if err := (tempoRuntime{}).Validate(base); err != nil {
		t.Fatalf("valid docker spec rejected: %v", err)
	}

	// Compose owns the lifecycle, so the per-node launcher must stay off.
	starting := base
	starting.StartNode = true
	if err := (tempoRuntime{}).Validate(starting); err == nil {
		t.Error("expected start_node=true to be rejected in docker mode")
	}

	// The docker launcher derives trusted peers from the OTHER validators, so a
	// single-node docker devnet boots with an empty --trusted-peers.
	single := base
	single.Validators = 1
	if err := (tempoRuntime{}).Validate(single); err == nil {
		t.Error("expected validators<2 to be rejected in docker mode")
	}

	noImage := base
	noImage.TempoDockerImage = ""
	if err := (tempoRuntime{}).Validate(noImage); err == nil {
		t.Error("expected a missing image to be rejected in docker mode")
	}
}

func TestTempoRejectsInsufficientGasForShape(t *testing.T) {
	base := messages.BenchmarkSpec{TxType: messages.ERC20TransferTx, ERC20TransferGas: 300000}
	// 300k covers a plain transfer...
	if err := (tempoRuntime{}).EnrichSpec(&base); err != nil {
		t.Errorf("300k gas must cover the default shape: %v", err)
	}
	// ...but not fresh or approve, which cost ~2x (state creation / allowance write).
	for _, shape := range []string{"fresh", "approve"} {
		spec := messages.BenchmarkSpec{TxType: messages.ERC20TransferTx, ERC20TransferGas: 300000, TempoTxShape: shape}
		if err := (tempoRuntime{}).EnrichSpec(&spec); err == nil {
			t.Errorf("shape %q must be rejected when erc20_transfer_gas is below its floor", shape)
		}
	}
	// A sufficient limit passes.
	freshOK := messages.BenchmarkSpec{TxType: messages.ERC20TransferTx, ERC20TransferGas: 600000, TempoTxShape: "fresh"}
	if err := (tempoRuntime{}).EnrichSpec(&freshOK); err != nil {
		t.Errorf("600k gas must cover fresh: %v", err)
	}
}

func TestTempoRejectsMultiNodeLegacyLoad(t *testing.T) {
	// The built-in signer derives node N from HD branch N, which tempo-xtask
	// never funds; only the native generator partitions the funded branch.
	legacy := messages.BenchmarkSpec{
		RunnerType: "local", Validators: 4, ValidatorGenerateLoad: true,
	}
	if err := (tempoRuntime{}).Validate(legacy); err == nil {
		t.Error("expected multi-node legacy load to be rejected as unfunded")
	}
	// Native generator partitions accounts, so multi-node is fine.
	native := legacy
	native.TempoTxGenerator = "/usr/bin/python3"
	if err := (tempoRuntime{}).Validate(native); err != nil {
		t.Errorf("native multi-node load must be allowed: %v", err)
	}
	// A single validator is unaffected either way.
	single := legacy
	single.Validators = 1
	if err := (tempoRuntime{}).Validate(single); err != nil {
		t.Errorf("single-validator legacy load must be allowed: %v", err)
	}
}

func TestTempoComposeProjectIsStableAndScoped(t *testing.T) {
	a := messages.BenchmarkSpec{DataDir: "/tmp/one"}
	b := messages.BenchmarkSpec{DataDir: "/tmp/two"}
	if tempoComposeProject(a) != tempoComposeProject(a) {
		t.Error("project name must be stable for a data dir")
	}
	if tempoComposeProject(a) == tempoComposeProject(b) {
		t.Error("different data dirs must not share a compose project")
	}
	explicit := messages.BenchmarkSpec{DataDir: "/tmp/one", TempoComposeProject: "pinned"}
	if got := tempoComposeProject(explicit); got != "pinned" {
		t.Errorf("explicit project ignored: %s", got)
	}
}

func TestTempoDockerPortsMatchComposePublishing(t *testing.T) {
	// The devnet publishes each node's JSON-RPC on base_port+4, and node
	// blocks are 6 ports apart; the benchmark must target the same ports.
	spec := messages.BenchmarkSpec{TempoBasePort: 8000}
	for seq, want := range map[int]int{0: 8004, 1: 8010, 2: 8016, 3: 8022} {
		if got := (tempoRuntime{}).EVMRPCPort(spec, seq); got != want {
			t.Errorf("node %d RPC port = %d, want %d", seq, got, want)
		}
	}
}

func TestTempoValidateRequiresTempoBin(t *testing.T) {
	spec := messages.BenchmarkSpec{RunnerType: "local", StartNode: true}
	if err := (tempoRuntime{}).Validate(spec); err == nil {
		t.Error("expected a missing tempo_bin to be rejected when start_node=true")
	}
}

// busyPort returns a port with a listener held open for the test's lifetime.
func busyPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

func TestTempoPreStartCheckGuardsOccupiedPort(t *testing.T) {
	port := busyPort(t)
	spec := messages.BenchmarkSpec{
		StartNode:     true,
		TempoBasePort: port - tempoHTTPPortOffset,
	}
	target := messages.NodeTarget{GlobalSeq: 0}

	if err := (tempoRuntime{}).PreStartCheck(spec, target); err == nil {
		t.Error("expected a busy port to block launching a node")
	}
	// An externally managed node is expected to already be listening.
	spec.StartNode = false
	if err := (tempoRuntime{}).PreStartCheck(spec, target); err != nil {
		t.Errorf("start_node=false must not require a free port: %v", err)
	}
	// Cosmos behaviour is unchanged: no pre-launch checks.
	if err := (cosmosRuntime{}).PreStartCheck(messages.BenchmarkSpec{StartNode: true}, target); err != nil {
		t.Errorf("cosmos pre-start check must stay a no-op: %v", err)
	}
}

func TestExternallyGeneratedGuardsRegeneration(t *testing.T) {
	// Native txs must never be re-signed by the legacy signer at run time.
	native := messages.BenchmarkSpec{ChainFamily: FamilyTempo, TempoTxGenerator: "/usr/bin/python3"}
	if !externallyGenerated(native) {
		t.Error("configured tempo generator must suppress legacy regeneration")
	}
	// Tempo without a generator uses the built-in signer, which may regenerate.
	builtin := messages.BenchmarkSpec{ChainFamily: FamilyTempo}
	if externallyGenerated(builtin) {
		t.Error("tempo without a generator must keep the built-in signer path")
	}
	// Cosmos is unaffected.
	if externallyGenerated(messages.BenchmarkSpec{ChainFamily: FamilyCosmos}) {
		t.Error("cosmos must keep its regeneration behaviour")
	}
}

func TestTempoProduceTxsInertWithoutGenerator(t *testing.T) {
	if (tempoRuntime{}).ProducesTxs(messages.BenchmarkSpec{}) {
		t.Error("no generator configured must fall back to the built-in signer")
	}
}

func TestTempoConsensusRPCIsAbsent(t *testing.T) {
	if (tempoRuntime{}).HasConsensusRPC() {
		t.Error("tempo exposes no CometBFT RPC; waiting on one would hang startup")
	}
	if !(cosmosRuntime{}).HasConsensusRPC() {
		t.Error("cosmos chains do expose a consensus RPC")
	}
}
