package activities

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// Port offsets within a reth node's port block.
const (
	rethP2PPortOffset     = 0
	rethAuthRPCPortOffset = 1
	rethHTTPPortOffset    = 2
	rethWSPortOffset      = 3
	rethMetricsPortOffset = 4
	rethPortsPerNode      = 5

	// defaultRethBasePort is clear of the cosmos (8545/26657), tempo (8000) and
	// allegro (9000) defaults.
	defaultRethBasePort = 9500

	rethDefaultNodeBin   = "reth"
	rethDefaultBlockTime = "1s"
)

var rethPorts = portBlock{
	defaultBase: defaultRethBasePort,
	size:        rethPortsPerNode,
	rpcOffset:   rethHTTPPortOffset,
}

// rethRuntime bootstraps a plain reth devnet (see reth_devnet.go): the validator
// is a `reth node --dev` that builds and validates every block, and each
// fullnode follows it through reth's debug consensus client, validating the same
// blocks through the engine API.
type rethRuntime struct{ devnetRuntime }

func (rethRuntime) Name() string { return FamilyReth }

// EnrichSpec points ERC-20 load at the token the genesis deploys, unless the
// operator names their own (with its code in genesis_patch.alloc).
func (rethRuntime) EnrichSpec(spec *messages.BenchmarkSpec) error {
	if spec.TxType == "" {
		spec.TxType = messages.SimpleTransferTx
	}
	if spec.TxType == messages.ERC20TransferTx && strings.TrimSpace(spec.ERC20ContractAddress) == "" {
		spec.ERC20ContractAddress = rethTokenAddress.Hex()
	}
	return nil
}

func (r rethRuntime) Bootstrap(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	if err := assertRPCPortsFree(r, spec, nodes); err != nil {
		return err
	}
	if err := generateRethDevnet(spec, nodes); err != nil {
		return fmt.Errorf("generate reth devnet: %w", err)
	}
	return nil
}

func (rethRuntime) Validate(spec messages.BenchmarkSpec) error {
	if spec.RunnerType == "docker" {
		return fmt.Errorf("chain_family=reth has no docker runner; use runner_type: local")
	}
	if spec.Validators != 1 {
		// A second dev node would build a competing chain.
		return fmt.Errorf("reth runs one block producer: set validators: 1 and add fullnodes to follow it (got validators: %d)",
			spec.Validators)
	}
	if spec.StartNode && strings.TrimSpace(spec.RethBin) == "" {
		return fmt.Errorf("reth_bin is required when start_node=true")
	}
	_, err := rethBlockTime(spec)
	return err
}

func (r rethRuntime) PreStartCheck(spec messages.BenchmarkSpec, target messages.NodeTarget) error {
	return devnetPreStartCheck(r, spec, target)
}

func (rethRuntime) EVMRPCPort(spec messages.BenchmarkSpec, globalSeq int) int {
	return rethPorts.rpc(spec.RethBasePort, globalSeq)
}

func (rethRuntime) MetricsURL(spec messages.BenchmarkSpec, globalSeq int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", rethPorts.base(spec.RethBasePort, globalSeq)+rethMetricsPortOffset)
}

// Metrics read by SummarizeMetrics. They are lifetime totals, durations in
// seconds; the empty blocks around the load add little time and no gas.
const (
	rethNewPayloadCount   = "reth_consensus_engine_beacon_new_payload_latency_count"
	rethNewPayloadSeconds = "reth_consensus_engine_beacon_new_payload_latency_sum"
	rethExecutionSeconds  = "reth_sync_execution_execution_histogram_sum"
	rethExecutionGas      = "reth_sync_execution_gas_processed_total"
	rethReuseReused       = "reth_sync_prewarm_reuse_reused_transactions"
	rethReuseMissing      = "reth_sync_prewarm_reuse_missing_transactions"
	rethReuseStale        = "reth_sync_prewarm_reuse_stale_transactions"
)

// SummarizeMetrics reports the node's block validation time and throughput and,
// with --engine.prewarm-reuse, how many transactions were committed from
// prewarm results (reused) or executed because the result was missing or stale.
// A missing metric is an error, so a renamed one cannot read as zero.
func (rethRuntime) SummarizeMetrics(metrics []byte) ([]string, error) {
	v := parsePrometheusText(metrics)
	for _, name := range []string{
		rethNewPayloadCount, rethNewPayloadSeconds, rethExecutionSeconds, rethExecutionGas,
		rethReuseReused, rethReuseMissing, rethReuseStale,
	} {
		if _, ok := v[name]; !ok {
			return nil, fmt.Errorf("reth metrics lack %s", name)
		}
	}
	mgasPerSecond := 0.0
	if seconds := v[rethExecutionSeconds]; seconds > 0 {
		mgasPerSecond = v[rethExecutionGas] / 1e6 / seconds
	}
	return []string{
		fmt.Sprintf("engine_summary new_payloads=%.0f new_payload_seconds=%.3f execution_seconds=%.3f gas=%.0f execution_mgas_per_s=%.1f",
			v[rethNewPayloadCount], v[rethNewPayloadSeconds], v[rethExecutionSeconds], v[rethExecutionGas], mgasPerSecond),
		fmt.Sprintf("reuse_summary reused=%.0f missing=%.0f stale=%.0f",
			v[rethReuseReused], v[rethReuseMissing], v[rethReuseStale]),
	}, nil
}

// parsePrometheusText returns a Prometheus text exposition's unlabelled samples
// by name; labelled ones (summary quantiles) are skipped.
func parsePrometheusText(text []byte) map[string]float64 {
	values := make(map[string]float64)
	scanner := bufio.NewScanner(bytes.NewReader(text))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || strings.Contains(fields[0], "{") {
			continue
		}
		if value, err := strconv.ParseFloat(fields[1], 64); err == nil {
			values[fields[0]] = value
		}
	}
	return values
}

// rethBlockTime returns the dev block interval, refusing one under a second:
// reth timestamps each block at least a second after its parent, so faster
// blocks run ahead of the clock and understate a rate measured from timestamps.
func rethBlockTime(spec messages.BenchmarkSpec) (time.Duration, error) {
	raw := cmp.Or(strings.TrimSpace(spec.RethBlockTime), rethDefaultBlockTime)
	blockTime, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("reth_block_time %q: %w", raw, err)
	}
	if blockTime < time.Second {
		return 0, fmt.Errorf("reth_block_time %s is under 1s, which understates the rate measured from block timestamps", blockTime)
	}
	return blockTime, nil
}
