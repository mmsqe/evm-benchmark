package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// Port offsets within an Allegro node's port block (0=consensus p2p,
// 1=execution p2p, 2=authrpc, 3=http JSON-RPC).
const (
	allegroExecP2PPortOffset = 1
	allegroAuthRPCPortOffset = 2
	allegroHTTPPortOffset    = 3
	allegroPortsPerNode      = 4

	// defaultAllegroBasePort is the first node's port block; chosen clear of the
	// cosmos (8545/26657) and tempo (8000) defaults so the two can coexist.
	defaultAllegroBasePort = 9000

	allegroDefaultNodeBin  = "allegro"
	allegroDefaultXtaskBin = "allegro-xtask"
)

// allegroPorts is Allegro's per-node port allocation.
var allegroPorts = portBlock{
	defaultBase: defaultAllegroBasePort,
	size:        allegroPortsPerNode,
	rpcOffset:   allegroHTTPPortOffset,
}

// allegroRuntime bootstraps an Allegro devnet (see allegro_devnet.go):
// `allegro-xtask genesis` writes genesis.json with the validator set embedded,
// the benchmark funds its own accounts into the genesis alloc and writes each
// node's launcher.
//
// Allegro is a reth execution node driven by commonware simplex consensus, so
// unlike Tempo it takes ordinary legacy/London transactions from the shared
// signer in internal/bench — there is no family-specific tx producer.
//
// Prerequisites (documented, not vendored): the `allegro` and `allegro-xtask`
// binaries from the allegro repo.
type allegroRuntime struct{}

func (allegroRuntime) Name() string { return FamilyAllegro }

// EnrichSpec applies Allegro-specific defaults. Allegro genesis deploys no
// contracts, so the ERC-20 shape is only usable when the operator supplies both
// a target address and its code (via genesis_patch); the native transfer shape
// is the default.
func (allegroRuntime) EnrichSpec(spec *messages.BenchmarkSpec) error {
	if spec.TxType == "" {
		spec.TxType = messages.SimpleTransferTx
	}
	if spec.TxType == messages.ERC20TransferTx && strings.TrimSpace(spec.ERC20ContractAddress) == "" {
		return fmt.Errorf(
			"tx_type %q on allegro requires erc20_contract_address (and its code in genesis_patch.alloc): "+
				"allegro genesis deploys no contracts, so transfers to an empty account would measure nothing",
			messages.ERC20TransferTx)
	}
	return nil
}

// Bootstrap materialises the network under spec.DataDir/devnet: genesis (with
// the benchmark's accounts funded) and a per-node home with a launcher.
func (a allegroRuntime) Bootstrap(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	if err := a.Validate(spec); err != nil {
		return err
	}
	if err := assertRPCPortsFree(a, spec, nodes); err != nil {
		return err
	}
	if err := generateAllegroDevnet(ctx, spec, nodes); err != nil {
		return fmt.Errorf("generate allegro devnet: %w", err)
	}
	return nil
}

// Validate rejects spec combinations the family cannot honour. There is no
// published allegro image and the cosmos docker runner is cosmos-shaped (it
// mounts /data/<group>/<seq>, maps CometBFT's 26657 and runs `<binary> start
// --home`), so docker must be refused rather than silently mis-run.
func (allegroRuntime) Validate(spec messages.BenchmarkSpec) error {
	if spec.RunnerType == "docker" {
		return fmt.Errorf("chain_family=allegro has no docker runner; use runner_type: local")
	}
	if spec.Fullnodes > 0 {
		// Every entry in an allegro genesis validator set votes; there is no
		// non-validating node type to generate a layout for.
		return fmt.Errorf("allegro runtime does not support fullnodes (got %d)", spec.Fullnodes)
	}
	if spec.StartNode && strings.TrimSpace(spec.AllegroBin) == "" {
		return fmt.Errorf("allegro_bin is required when start_node=true")
	}
	return nil
}

// PreStartCheck refuses to launch when the node's RPC port is already served,
// which would otherwise mean benchmarking a leftover node's stale genesis.
func (a allegroRuntime) PreStartCheck(spec messages.BenchmarkSpec, target messages.NodeTarget) error {
	if !spec.StartNode {
		return nil // an externally managed node is expected to be listening
	}
	return assertRPCPortsFree(a, spec, []messages.NodeTarget{target})
}

// HasConsensusRPC reports false: Allegro exposes no CometBFT-style RPC, so node
// readiness is determined from the EVM JSON-RPC port alone.
func (allegroRuntime) HasConsensusRPC() bool { return false }

// EVMRPCPort returns the node's HTTP JSON-RPC port from its port block.
func (allegroRuntime) EVMRPCPort(spec messages.BenchmarkSpec, globalSeq int) int {
	return allegroPorts.rpc(spec.AllegroBasePort, globalSeq)
}

// LocalStartCommand runs the launcher generated for the node (see
// allegroRunScript). The node home doubles as the reth datadir (`--datadir .`).
func (allegroRuntime) LocalStartCommand(spec messages.BenchmarkSpec, target messages.NodeTarget) ([]string, string) {
	return devnetLaunchCommand(spec, target)
}

// allegroBasePort returns the base of a node's 4-port block.
func allegroBasePort(spec messages.BenchmarkSpec, globalSeq int) int {
	return allegroPorts.base(spec.AllegroBasePort, globalSeq)
}
