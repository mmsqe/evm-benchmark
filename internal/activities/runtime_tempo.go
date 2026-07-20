package activities

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mmsqe/evm-benchmark/internal/bench"

	"github.com/mmsqe/evm-benchmark/internal/messages"
	"gopkg.in/yaml.v3"
)

// Port offsets within a Tempo node's port block, mirroring tempo-py's
// tempo/devnet/ports.py (0=consensus-p2p, 1=execution-p2p, 2=metrics,
// 3=authrpc, 4=http, 5=ws).
const (
	tempoHTTPPortOffset = 4
	tempoPortsPerNode   = 6

	// defaultTempoBasePort is the first node's port block; chosen high to
	// avoid colliding with the cosmos defaults.
	defaultTempoBasePort = 8000

	// tempoDefaultFeeToken is the TIP-20 fee token present in every Tempo
	// genesis, usable as an ERC-20 transfer target without deploying anything.
	tempoDefaultFeeToken = "0x20c0000000000000000000000000000000000000"

	// tempoMinTxGas is Tempo's intrinsic gas floor (~21k + ~250k); anything
	// below is rejected with "call gas cost exceeds the gas limit".
	tempoMinTxGas = 272000
)

// tempoShapeMinGas is the measured per-transaction gas floor for shapes that
// cost more than a plain transfer (see plan.md step 9), so a config can be
// rejected up front instead of every transaction being dropped at runtime.
// Shapes not listed here are covered by tempoMinTxGas.
var tempoShapeMinGas = map[string]uint64{
	"multitoken":       320000, // ~4 transfers in one tx
	"batch":            300000, // ~271k + a few k per extra call (4 by default)
	"fresh":            550000, // state creation ~doubles the cost
	"approve":          550000, // allowance-slot write is expensive
	"memo":             300000, // transfer plus a 32-byte memo
	"approve_transfer": 320000, // approve + transferFrom in one tx
}

// tempoRuntime bootstraps a Tempo devnet by delegating to tempo-py's
// `tempo-devnet init`, which generates genesis, consensus keys, enode
// identities and a per-node run.sh. Tempo is not a cosmos-sdk chain, so none
// of the init/gentx/bech32 machinery applies.
//
// Prerequisites (documented, not vendored): tempo-py's `tempo-devnet` on PATH
// (or via TempoDevnetBin) plus `tempo` and `tempo-xtask` binaries.
type tempoRuntime struct{}

func (tempoRuntime) Name() string { return FamilyTempo }

// EnrichSpec applies Tempo-specific defaults. Tempo forbids native value
// transfers and charges a ~271k intrinsic gas floor, so the generator must use
// the ERC-20 (TIP-20) transaction shape with a sufficient gas limit; see
// plan.md "Step 1 results".
func (tempoRuntime) EnrichSpec(spec *messages.BenchmarkSpec) error {
	if spec.TxType == messages.SimpleTransferTx {
		return fmt.Errorf(
			"tx_type %q is not supported on Tempo (native value transfers are rejected); use %q",
			messages.SimpleTransferTx, messages.ERC20TransferTx)
	}
	if spec.TxType == "" {
		spec.TxType = messages.ERC20TransferTx
	}
	if spec.ERC20ContractAddress == "" {
		spec.ERC20ContractAddress = tempoDefaultFeeToken
	}
	minGas := uint64(tempoMinTxGas)
	if shape := strings.TrimSpace(spec.TempoTxShape); shape != "" {
		if shapeGas, ok := tempoShapeMinGas[shape]; ok {
			minGas = shapeGas
		}
	}
	if spec.ERC20TransferGas < minGas {
		return fmt.Errorf(
			"erc20_transfer_gas %d is below the floor for tx_shape %q (%d); transactions would be rejected",
			spec.ERC20TransferGas, spec.TempoTxShape, minGas)
	}
	return nil
}

// Bootstrap writes a devnet.yaml describing the requested validators and runs
// `tempo-devnet init` to materialise the network under spec.DataDir.
func (t tempoRuntime) Bootstrap(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	if spec.Fullnodes > 0 {
		return fmt.Errorf("tempo runtime does not support fullnodes yet (got %d)", spec.Fullnodes)
	}
	if spec.RunnerType == "docker" {
		// Bootstrap runs more than once per benchmark (benchctl gen, then the
		// GenerateLayout activity), and in docker mode it starts the cluster,
		// so it must be idempotent: tear down this project's containers before
		// regenerating. The stale-node port guard below does not apply here
		// because compose owns these ports and we have just released them.
		// A missing compose file on the first run is expected, so a failure
		// here is not fatal.
		_, _ = chainCmd(ctx, "docker", nil, tempoComposeArgs(spec, "down", "-t", "5")...)
	} else {
		// A node left over from an earlier run keeps serving its own (stale)
		// genesis on the same port. Load would then silently measure that chain
		// instead of the one generated here, so refuse to continue.
		for _, node := range nodes {
			port := t.EVMRPCPort(spec, node.GlobalSeq)
			if err := assertPortFree(port); err != nil {
				return err
			}
		}
	}

	validators := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		validators = append(validators, map[string]any{
			"host":    "127.0.0.1",
			"port":    tempoBasePort(spec, node.GlobalSeq),
			"moniker": tempoMoniker(node.GlobalSeq),
		})
	}

	devnet := map[string]any{
		"chain_id": spec.EVMChainID,
		// Every node takes a disjoint slice of the funded branch, plus index 0
		// which is reserved for the validator key.
		"accounts":   tempoFundedAccounts(spec),
		"mnemonic":   spec.BaseMnemonic,
		"validators": validators,
	}
	if spec.TempoEpochLength > 0 {
		devnet["epoch_length"] = spec.TempoEpochLength
	}
	if spec.TempoGasLimit > 0 {
		devnet["gas_limit"] = spec.TempoGasLimit
	}
	if spec.TempoBin != "" {
		devnet["tempo_bin"] = spec.TempoBin
	}
	if spec.TempoXtaskBin != "" {
		devnet["tempo_xtask_bin"] = spec.TempoXtaskBin
	}
	if len(spec.GenesisPatch) > 0 {
		devnet["patch_genesis"] = spec.GenesisPatch
	}
	if len(spec.ConfigPatch) > 0 {
		devnet["patch_reth"] = spec.ConfigPatch
	}
	if spec.RunnerType == "docker" {
		devnet["docker"] = map[string]any{
			"image":   spec.TempoDockerImage,
			"network": tempoDockerNetwork(spec),
		}
	}

	encoded, err := yaml.Marshal(devnet)
	if err != nil {
		return fmt.Errorf("encode devnet config: %w", err)
	}
	configPath := filepath.Join(spec.DataDir, "devnet.yaml")
	if err := os.WriteFile(configPath, encoded, 0o644); err != nil {
		return fmt.Errorf("write devnet config: %w", err)
	}

	devnetBin := spec.TempoDevnetBin
	if devnetBin == "" {
		devnetBin = "tempo-devnet"
	}
	dataDir := filepath.Join(spec.DataDir, "devnet")
	initArgs := []string{"init", "--data", dataDir, "--config", configPath, "--force"}
	if spec.RunnerType == "docker" {
		// Emits docker-compose.yaml alongside the genesis and per-node dirs.
		initArgs = append(initArgs, "--gen-compose-file")
	}
	if _, err := chainCmd(ctx, devnetBin, nil, initArgs...); err != nil {
		return fmt.Errorf("tempo-devnet init: %w", err)
	}

	if spec.RunnerType == "docker" {
		if _, err := chainCmd(ctx, "docker", nil, tempoComposeArgs(spec, "up", "-d")...); err != nil {
			return fmt.Errorf("docker compose up: %w", err)
		}
	}
	return nil
}

// tempoFundedAccounts is how many accounts genesis must fund: one disjoint
// slice of NumAccounts per node, plus index 0 for the validator key.
func tempoFundedAccounts(spec messages.BenchmarkSpec) int {
	return spec.NumAccounts*max(spec.Validators, 1) + 1
}

func tempoDockerNetwork(spec messages.BenchmarkSpec) string {
	if spec.TempoDockerNetwork != "" {
		return spec.TempoDockerNetwork
	}
	return "tempo-devnet"
}

// tempoComposeProject keeps concurrent benchmark data dirs from colliding:
// compose otherwise derives the project name from the directory basename.
func tempoComposeProject(spec messages.BenchmarkSpec) string {
	if spec.TempoComposeProject != "" {
		return spec.TempoComposeProject
	}
	sum := sha1.Sum([]byte(spec.DataDir))
	return fmt.Sprintf("evm-benchmark-tempo-%x", sum[:4])
}

// tempoComposeArgs builds a `docker compose` invocation against the compose
// file `tempo-devnet init --gen-compose-file` writes beside the network.
func tempoComposeArgs(spec messages.BenchmarkSpec, args ...string) []string {
	composeFile := filepath.Join(spec.DataDir, "devnet", "docker-compose.yaml")
	return append([]string{"compose", "-p", tempoComposeProject(spec), "-f", composeFile}, args...)
}

// Validate rejects unsupported spec combinations. The docker runner is
// cosmos-shaped (it mounts /data/<group>/<seq> and maps CometBFT's 26657 plus
// 8545, and launches `<binary> start --home`), none of which matches a Tempo
// devnet, so it must be refused rather than silently mis-run.
func (tempoRuntime) Validate(spec messages.BenchmarkSpec) error {
	if spec.RunnerType == "docker" {
		// Compose owns the container lifecycle for the whole cluster, so the
		// per-node launcher must stay out of the way. RunNode then waits for
		// the published RPC and sends load, exactly as it would for any
		// externally managed node.
		if spec.StartNode {
			return fmt.Errorf("chain_family=tempo with runner_type=docker manages nodes via docker compose; set start_node: false")
		}
		if spec.TempoDockerImage == "" {
			return fmt.Errorf("tempo_docker_image is required for runner_type=docker")
		}
		if spec.Validators < 2 {
			// tempo-devnet builds each container's trusted-peers list from the
			// *other* validators, so a single-node docker devnet is started
			// with an empty --trusted-peers and the node refuses to boot.
			return fmt.Errorf("chain_family=tempo with runner_type=docker needs validators >= 2 (got %d); use runner_type: local for a single node", spec.Validators)
		}
		return nil
	}
	if spec.StartNode && spec.TempoBin == "" {
		return fmt.Errorf("tempo_bin is required when start_node=true")
	}
	if spec.TempoTxGenerator == "" && spec.Validators > 1 && spec.ValidatorGenerateLoad {
		// The built-in signer derives node N's accounts from HD branch N, but
		// tempo-xtask funds only branch 0, so every node above the first would
		// send from unfunded accounts and its load would be rejected. The
		// native generator avoids this by giving each node a disjoint slice of
		// branch 0.
		return fmt.Errorf(
			"multi-node load with the built-in signer would use unfunded accounts on nodes 1..%d; "+
				"set tempo_tx_generator (native txs) or run a single validator", spec.Validators-1)
	}
	return nil
}

// PreStartCheck refuses to launch when the node's RPC port is already served,
// which would otherwise mean benchmarking a leftover node's stale genesis.
func (t tempoRuntime) PreStartCheck(spec messages.BenchmarkSpec, target messages.NodeTarget) error {
	if !spec.StartNode || spec.RunnerType == "docker" {
		// An externally managed node — including a compose-managed container —
		// is expected to already be listening.
		return nil
	}
	return assertPortFree(t.EVMRPCPort(spec, target.GlobalSeq))
}

// HasConsensusRPC reports false: Tempo exposes no CometBFT-style RPC, so node
// readiness is determined from the EVM JSON-RPC port alone.
func (tempoRuntime) HasConsensusRPC() bool { return false }

// EVMRPCPort returns the node's HTTP JSON-RPC port from its port block.
func (tempoRuntime) EVMRPCPort(spec messages.BenchmarkSpec, globalSeq int) int {
	return tempoBasePort(spec, globalSeq) + tempoHTTPPortOffset
}

// LocalStartCommand runs the launcher tempo-devnet generated for the node; it
// resolves its own relative paths, so it must run from the node home.
func (tempoRuntime) LocalStartCommand(spec messages.BenchmarkSpec, target messages.NodeTarget) ([]string, string) {
	home := tempoNodeHome(spec, target.GlobalSeq)
	return []string{filepath.Join(home, "run.sh")}, home
}

// tempoBasePort returns the base of a node's 6-port block. Node targets are
// addressed on their HTTP JSON-RPC port, which sits at offset 4.
func tempoBasePort(spec messages.BenchmarkSpec, globalSeq int) int {
	base := spec.TempoBasePort
	if base == 0 {
		base = defaultTempoBasePort
	}
	return base + globalSeq*tempoPortsPerNode
}

func tempoMoniker(globalSeq int) string {
	return fmt.Sprintf("node%d", globalSeq)
}

// tempoNodeHome is the per-node directory `tempo-devnet init` creates.
func tempoNodeHome(spec messages.BenchmarkSpec, globalSeq int) string {
	return filepath.Join(spec.DataDir, "devnet", tempoMoniker(globalSeq))
}

// assertPortFree fails when something already listens on the node's JSON-RPC
// port, which would otherwise be mistaken for a healthy freshly-started node.
func assertPortFree(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return nil // nothing listening: the port is ours to use
	}
	_ = conn.Close()
	return fmt.Errorf(
		"port %d is already in use: a node from a previous run is still serving its own genesis; "+
			"stop it first (e.g. scripts/run-benchmark.sh --mode tempo stop)", port)
}

// ProducesTxs reports whether an external generator is configured.
func (tempoRuntime) ProducesTxs(spec messages.BenchmarkSpec) bool {
	return strings.TrimSpace(spec.TempoTxGenerator) != ""
}

// ProduceTxs generates the node's transaction file with an external generator
// (see scripts/gen_tempo_txs.py), which emits Tempo's native 0x76 envelope via
// tempo-py's canonical encoder. Only called when ProducesTxs reports true.
//
// The generator derives keys with the same path as internal/keygen, so a chain
// funded for the built-in signer is funded for this one.
func (tempoRuntime) ProduceTxs(
	ctx context.Context,
	spec messages.BenchmarkSpec,
	target messages.NodeTarget,
	txPath string,
) (int, error) {
	generator := strings.TrimSpace(spec.TempoTxGenerator)

	token := spec.ERC20ContractAddress
	if token == "" {
		token = tempoDefaultFeeToken
	}
	args := append([]string{}, spec.TempoTxGeneratorArgs...)
	args = append(args,
		"--out", txPath,
		// tempo-xtask funds only the m/44'/60'/0'/0/i branch, so every node
		// draws from that branch and takes a disjoint slice of it instead of
		// using its own (unfunded) branch the way the legacy signer does.
		"--global-seq", "0",
		"--account-offset", strconv.Itoa(target.GlobalSeq*spec.NumAccounts),
		"--accounts", strconv.Itoa(spec.NumAccounts),
		"--txs-per-account", strconv.Itoa(spec.NumTxs),
		"--chain-id", strconv.FormatInt(spec.EVMChainID, 10),
		"--mnemonic", spec.BaseMnemonic,
		"--token", token,
		"--gas-limit", strconv.FormatUint(spec.ERC20TransferGas, 10),
		"--max-fee-per-gas", strconv.FormatInt(spec.GasPriceWei, 10),
		"--max-priority-fee-per-gas", strconv.FormatInt(spec.TempoMaxPriorityFeePerGas, 10),
		"--nonce-key", strconv.Itoa(spec.TempoNonceKey),
		// Gas is always paid in the genesis fee token: a custom transfer target
		// is not necessarily a valid or funded fee token.
		"--fee-token", tempoDefaultFeeToken,
		// One GenerateTxs activity runs per node concurrently, so let each
		// generator use a fair share of the host instead of cpu_count each.
		"--workers", strconv.Itoa(tempoSigningWorkers(spec)),
	)
	if shape := strings.TrimSpace(spec.TempoTxShape); shape != "" {
		args = append(args, "--tx-shape", shape)
	}
	if spec.TempoBatchCalls > 0 {
		args = append(args, "--batch-calls", strconv.Itoa(spec.TempoBatchCalls))
	}

	out, err := chainCmd(ctx, generator, nil, args...)
	if err != nil {
		return 0, fmt.Errorf("generate native tempo txs: %w", err)
	}

	// The generator reports a JSON summary on stdout; fall back to counting the
	// file if that ever changes, rather than failing the run over a log line.
	var summary struct {
		Txs int `json:"txs"`
	}
	// The summary is the final line: incidental generator output (warnings,
	// progress) precedes it and must not break parsing.
	trimmed := strings.TrimSpace(string(out))
	lastLine := trimmed[strings.LastIndexByte(trimmed, '\n')+1:]
	if jsonErr := json.Unmarshal([]byte(lastLine), &summary); jsonErr != nil || summary.Txs == 0 {
		var raws []string
		if readErr := readJSON(txPath, &raws); readErr != nil {
			return 0, fmt.Errorf("read generated tempo txs: %w", readErr)
		}
		if len(raws) == 0 {
			return 0, fmt.Errorf("generator %q produced no transactions", generator)
		}
		return len(raws), nil
	}
	return summary.Txs, nil
}

// tempoSigningWorkers divides the host between the per-node generators that
// run concurrently, mirroring the headroom the built-in Go signer reserves.
func tempoSigningWorkers(spec messages.BenchmarkSpec) int {
	nodes := max(spec.Validators, 1)
	workers := max(runtime.GOMAXPROCS(0)-bench.SigningHeadroomReserved, 1) / nodes
	return max(workers, 1)
}
