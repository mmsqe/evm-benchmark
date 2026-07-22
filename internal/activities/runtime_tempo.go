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
	"strings"
	"time"

	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// Port offsets within a Tempo node's port block (0=consensus-p2p,
// 1=execution-p2p, 2=metrics, 3=authrpc, 4=http, 5=ws).
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
// cost more than a plain transfer, so a config can be rejected
// up front instead of every transaction being dropped at runtime.
// Shapes not listed here are covered by tempoMinTxGas.
var tempoShapeMinGas = map[string]uint64{
	"multitoken":       320000, // ~4 transfers in one tx
	"batch":            300000, // ~271k + a few k per extra call (4 by default)
	"fresh":            550000, // state creation ~doubles the cost
	"approve":          550000, // allowance-slot write is expensive
	"memo":             300000, // transfer plus a 32-byte memo
	"approve_transfer": 320000, // approve + transferFrom in one tx
}

// tempoRuntime bootstraps a Tempo devnet in-process (see tempo_devnet.go):
// `tempo-xtask generate-localnet` builds genesis, consensus keys and enode
// identities, and the benchmark itself wires trusted peers and writes each
// node's launcher. Tempo is not a cosmos-sdk chain, so none of the
// init/gentx/bech32 machinery applies.
//
// Prerequisites (documented, not vendored): the `tempo` and `tempo-xtask`
// binaries from the tempo repo.
type tempoRuntime struct{}

func (tempoRuntime) Name() string { return FamilyTempo }

// EnrichSpec applies Tempo-specific defaults. Tempo forbids native value
// transfers and charges a ~271k intrinsic gas floor, so the generator must use
// the ERC-20 (TIP-20) transaction shape with a sufficient gas limit.
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

// Bootstrap materialises the network under spec.DataDir/devnet, generating
// genesis, keys and per-node launchers in-process (see generateTempoDevnet).
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

	// Generate genesis, keys and per-node launchers in-process (see
	// tempo_devnet.go): tempo-xtask builds genesis and consensus keys, and the
	// rest of the network wiring is done here.
	if err := generateTempoDevnet(ctx, spec, nodes); err != nil {
		return fmt.Errorf("generate tempo devnet: %w", err)
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
// file writeTempoCompose writes beside the generated network.
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
			// The docker launcher builds each container's trusted-peers list from
			// the *other* validators, so a single-node docker devnet is started
			// with an empty --trusted-peers and the node refuses to boot.
			return fmt.Errorf("chain_family=tempo with runner_type=docker needs validators >= 2 (got %d); use runner_type: local for a single node", spec.Validators)
		}
		return nil
	}
	if spec.StartNode && spec.TempoBin == "" {
		return fmt.Errorf("tempo_bin is required when start_node=true")
	}
	if spec.TempoLegacyTxs && spec.Validators > 1 && spec.ValidatorGenerateLoad {
		// The legacy signer derives node N's accounts from HD branch N, but
		// tempo-xtask funds only branch 0, so every node above the first would
		// send from unfunded accounts and its load would be rejected. The native
		// generator (the default) avoids this by giving each node a disjoint
		// slice of branch 0.
		return fmt.Errorf(
			"multi-node load with tempo_legacy_txs would use unfunded accounts on nodes 1..%d; "+
				"use the native generator (unset tempo_legacy_txs) or run a single validator", spec.Validators-1)
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

// LocalStartCommand runs the launcher generated for the node (see
// tempoRunScript); it resolves its own relative paths, so it must run from the
// node home.
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

// tempoNodeHome is the per-node directory generateTempoDevnet creates.
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

// ProducesTxs reports whether the runtime signs the node's transactions itself.
// True for the native 0x76 path (the default); false when tempo_legacy_txs opts
// into the shared legacy/London signer in internal/bench.
func (tempoRuntime) ProducesTxs(spec messages.BenchmarkSpec) bool {
	return !spec.TempoLegacyTxs
}

// ProduceTxs writes the node's transaction file as Tempo's native 0x76 envelope
// (see internal/tempotx and generateTempoNativeTxs), signed in-process. Only
// called when ProducesTxs reports true.
//
// Keys are derived with the same path as internal/keygen, so a chain funded for
// the built-in signer is funded for this one.
func (tempoRuntime) ProduceTxs(
	ctx context.Context,
	spec messages.BenchmarkSpec,
	target messages.NodeTarget,
	txPath string,
) (int, error) {
	raws, err := generateTempoNativeTxs(ctx, spec, target)
	if err != nil {
		return 0, fmt.Errorf("generate native tempo txs: %w", err)
	}
	encoded, err := json.Marshal(raws)
	if err != nil {
		return 0, fmt.Errorf("encode tempo txs: %w", err)
	}
	// Write via a temp file so a crash cannot leave truncated JSON where a
	// previously valid transaction file used to be.
	tmp := txPath + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		return 0, fmt.Errorf("write tempo txs: %w", err)
	}
	if err := os.Rename(tmp, txPath); err != nil {
		return 0, fmt.Errorf("finalize tempo txs: %w", err)
	}
	return len(raws), nil
}

// tempoSigningWorkers divides the host between the per-node generators that
// run concurrently, mirroring the headroom the built-in Go signer reserves.
func tempoSigningWorkers(spec messages.BenchmarkSpec) int {
	nodes := max(spec.Validators, 1)
	workers := max(runtime.GOMAXPROCS(0)-bench.SigningHeadroomReserved, 1) / nodes
	return max(workers, 1)
}
