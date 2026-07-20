package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// Chain families supported by the benchmark. The family selects how a network
// is bootstrapped and launched; the load path (internal/bench) is shared.
const (
	FamilyCosmos = "cosmos"
	FamilyTempo  = "tempo"
)

// ChainRuntime abstracts everything that differs between chain families:
// turning a spec into a running network. Transaction generation, broadcast and
// stats are family-agnostic and live in internal/bench.
type ChainRuntime interface {
	// Name returns the family identifier.
	Name() string

	// EnrichSpec fills spec fields from the chain profile before the layout is
	// generated (chain id, denom, genesis patches, ...).
	EnrichSpec(spec *messages.BenchmarkSpec) error

	// Bootstrap creates node homes, keys and genesis for the given targets.
	Bootstrap(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error

	// HasConsensusRPC reports whether nodes expose a consensus RPC endpoint
	// (CometBFT's 26657) that must be ready before load starts. Tempo has no
	// such endpoint: readiness is determined from the EVM RPC alone.
	HasConsensusRPC() bool

	// EVMRPCPort returns the host port serving EVM JSON-RPC for a node.
	EVMRPCPort(spec messages.BenchmarkSpec, globalSeq int) int

	// LocalStartCommand returns the argv launching a node in local (non-docker)
	// mode, and the working directory for it ("" keeps the caller's).
	LocalStartCommand(spec messages.BenchmarkSpec, target messages.NodeTarget) (argv []string, dir string)

	// Validate rejects spec combinations the family cannot honour, so a
	// misconfiguration fails fast instead of silently measuring the wrong
	// thing.
	Validate(spec messages.BenchmarkSpec) error

	// PreStartCheck runs immediately before this node is launched. Bootstrap
	// is skipped when an existing layout is reused (skip_generate_layout), so
	// per-node safety checks belong here rather than only in Bootstrap.
	PreStartCheck(spec messages.BenchmarkSpec, target messages.NodeTarget) error
}

// txProducer is implemented by runtimes that can generate a node's transaction
// file themselves instead of using the shared legacy signer in internal/bench.
type txProducer interface {
	// ProducesTxs reports whether this spec will actually use the producer.
	// False means "not configured": the caller falls back to the built-in
	// generator, so the cosmos path is untouched. Callers also use it to decide
	// whether transactions may be re-signed by the built-in signer, which would
	// destroy a native envelope.
	ProducesTxs(spec messages.BenchmarkSpec) bool

	// ProduceTxs writes the node's transaction file. Only called when
	// ProducesTxs reports true.
	ProduceTxs(ctx context.Context, spec messages.BenchmarkSpec, target messages.NodeTarget, txPath string) (count int, err error)
}

// resolveRuntime returns the runtime for the spec's chain family, defaulting to
// cosmos so existing configs keep working unchanged.
func resolveRuntime(spec messages.BenchmarkSpec) (ChainRuntime, error) {
	switch family := strings.TrimSpace(spec.ChainFamily); family {
	case "", FamilyCosmos:
		return cosmosRuntime{}, nil
	case FamilyTempo:
		return tempoRuntime{}, nil
	default:
		return nil, fmt.Errorf("unknown chain_family %q", family)
	}
}

// cosmosRuntime bootstraps cosmos-sdk chains (evmd, mantrachaind) using the
// chain binary's own init/gentx/genesis commands.
type cosmosRuntime struct{}

func (cosmosRuntime) Name() string { return FamilyCosmos }

func (cosmosRuntime) EnrichSpec(spec *messages.BenchmarkSpec) error {
	return enrichGenesisPatchFromChainConfig(spec)
}

func (cosmosRuntime) Bootstrap(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	return bootstrapNodesAndGenesis(ctx, spec, nodes)
}

func (cosmosRuntime) HasConsensusRPC() bool { return true }

func (cosmosRuntime) EVMRPCPort(spec messages.BenchmarkSpec, globalSeq int) int {
	if spec.RunnerType == "docker" {
		return spec.EVMRPCPort + globalSeq
	}
	return spec.EVMRPCPort
}

func (cosmosRuntime) Validate(spec messages.BenchmarkSpec) error {
	if spec.StartNode && strings.TrimSpace(spec.Binary) == "" {
		return fmt.Errorf("benchmark.binary is required when start_node=true")
	}
	return nil
}

// PreStartCheck keeps the historical cosmos behaviour: no pre-launch checks.
func (cosmosRuntime) PreStartCheck(messages.BenchmarkSpec, messages.NodeTarget) error { return nil }

func (cosmosRuntime) LocalStartCommand(spec messages.BenchmarkSpec, target messages.NodeTarget) ([]string, string) {
	argv := []string{spec.Binary, "start", "--home", target.Home}
	if strings.TrimSpace(spec.ChainID) != "" {
		argv = append(argv, "--chain-id", spec.ChainID)
	}
	return append(argv, spec.StartArgs...), ""
}
