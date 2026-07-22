# evm-benchmark

Temporal-based stateless EVM load testing in Go.

## Implemented Flow

1. `gen`: create benchmark data layout and node targets.
2. `patchimage`:
   - docker mode: build a derived image that contains generated benchmark data.
   - local mode: copy a prepared data layout into `benchmark.data_dir`.
3. `gen_txs`: pre-generate deterministic signed EVM transactions per node.
4. `run`: for each node, send load, detect idle/halt, write `block_stats.log`.

## Project Layout

- `cmd/worker`: Temporal worker that runs benchmark activities.
- `cmd/starter`: CLI to start and await benchmark workflow completion.
- `cmd/benchctl`: local utility CLI for `build-image`, `gen`, and `patchimage`.
- `internal/workflows/stateless.go`: orchestration of `gen -> gen_txs -> run`.
- `internal/activities/stateless.go`: concrete activity implementations.
- `internal/bench`: EVM tx generation, JSON-RPC send, idle/halt detection, TPS stats.
- `config/chains.jsonnet`: local chain profiles used by `benchmark.chain_config`.
- `examples/config.yaml`: runnable config sample.

## Docker Mode (default)

Use `scripts/run-benchmark.sh` for the standard docker workflow.

```bash
export CHAIN_CONFIG=evmd
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"

# One command for stop(old runtime) + clean + prepare + temporal + worker + starter.
scripts/run-benchmark.sh run

# Optional: pin a specific cosmos/evm ref during run.
# scripts/run-benchmark.sh --commit-sha <sha-or-ref> run

# Stop benchmark runtime (workflows + worker + temporal + containers).
# scripts/run-benchmark.sh stop
```

Results are written to `benchmark.out_dir` in `examples/config.yaml`.

## Tempo Mode

Benchmarks a [Tempo](https://github.com/tempoxyz/tempo) devnet (commonware
consensus) with the same generator and stats used for the cosmos chains, so
results are comparable.

### Prerequisites

1. `tempo` and `tempo-xtask` binaries — build them in the tempo repo:
   `cargo build --bin tempo --bin tempo-xtask` (add `--release` for real runs).
   These are the only external binaries: the devnet and the native `0x76`
   transactions are both generated in-process.
2. The `temporal` CLI (the run script starts a dev server itself).

### Configure

Point `examples/config.tempo.yaml` at your toolchain — these paths are the only
machine-specific settings:

```yaml
binary:          /path/to/tempo/target/release/tempo
tempo_bin:       /path/to/tempo/target/release/tempo
tempo_xtask_bin: /path/to/tempo/target/release/tempo-xtask
```

The docker profile (`examples/config.tempo.docker.yaml`) needs the same, except
`tempo_bin` is the in-image command name.

### Run

```bash
scripts/run-benchmark.sh --mode tempo run
```

That stops any previous runtime, generates the devnet, pre-signs the
transactions, starts the node, sends the load, and writes
`/tmp/tempo-benchmark/output/node_0_block_stats.log`. Stop everything with
`scripts/run-benchmark.sh --mode tempo stop`.

The line that matters is `tx_summary`: if `included` is far below `sent`, the
run measured rejection, not throughput.

### Scale the load

The default (500 accounts x 40 txs) drains in about a second, which is too fast
to measure. For a real number use a load large enough to span many blocks:

```bash
sed -e 's/num_accounts: 500/num_accounts: 2000/' -e 's/num_txs: 40/num_txs: 100/' \
  examples/config.tempo.yaml > /tmp/config.tempo.large.yaml
scripts/run-benchmark.sh --mode tempo --config /tmp/config.tempo.large.yaml run
```

### Transaction shape

By default the load is Tempo's **native `0x76`** envelope, signed in-process by
the Go encoder in `internal/tempotx` (byte-verified against Tempo's canonical
encoding). `tempo_tx_shape` selects the workload (`self`, `hot`, `noop`,
`batch`, `fresh`, `multitoken`, `approve`, `memo`, `approve_transfer`) — see the
`plan.md` shapes table for what each touches and its gas floor. Heavier shapes
need a higher `erc20_transfer_gas`, which is enforced up front.

Set `tempo_legacy_txs: true` to fall back to legacy/London EVM transactions
(Tempo's compatibility path) — but only for a single validator: the legacy
signer derives node *N*'s accounts from an HD branch `tempo-xtask` does not
fund, so multi-node legacy load is rejected.

Note that Tempo rejects native value transfers (so `tx_type` is
`erc20-transfer`), charges a ~271k intrinsic gas floor, and has no CometBFT RPC
or `chains.jsonnet` profile — the network is described by the `tempo_*` fields.

### Docker mode

Runs the validators as containers. The devnet (including a
`docker-compose.yaml`) is generated in-process and started with
`docker compose up -d`, so compose owns the lifecycle:

```bash
docker pull ghcr.io/tempoxyz/tempo:latest
scripts/run-benchmark.sh --mode tempo-docker run
scripts/run-benchmark.sh --mode tempo-docker stop   # tears the cluster down
```

Constraints, all enforced with clear errors:

- `start_node: false` — compose starts the nodes, not the benchmark;
- `validators: >= 2` — the docker launcher derives each container's trusted
  peers from the *other* validators, so a single-node docker devnet gets an
  empty `--trusted-peers` and will not boot;
- `tempo_bin` is the in-image command name (`tempo`), while `tempo_xtask_bin`
  runs on the host (tx signing is in-process).

Each node draws a disjoint slice of the funded account branch, so genesis funds
`validators * num_accounts + 1` accounts (index 0 is the validator key). Note that this measures the tempo build inside the
image, which is usually not the binary you built locally.

### Troubleshooting

Verify the accounts the generator signs from can actually pay, against the RPC
you are about to benchmark (node0 serves `base_port + 4`):

```bash
go run ./cmd/checkfunding -rpc http://127.0.0.1:8004 -chain-id 1337
```

In local mode, a node left over from an earlier run serves its own stale
genesis; bootstrap refuses to start in that case rather than silently
benchmarking the wrong chain. Run the `stop` command above if you hit it.
Docker mode instead tears its own compose project down before recreating it,
so it is idempotent.

### Tests

```bash
TEMPO_BIN=/path/to/tempo \
TEMPO_XTASK_BIN=/path/to/tempo-xtask \
go test ./internal/activities -run Tempo
```

The devnet-bootstrapping test skips unless those two are set; the rest of the
suite (including the byte-for-byte encoder check in `internal/tempotx`) runs
unconditionally.

See `plan.md` for measured Tempo characteristics and results.

## Local Mode

Use local mode when you do not want Docker node runtime.

```bash
export CHAIN_CONFIG=evmd

# One command for stop(old runtime) + clean + prepare + temporal + worker + starter.
scripts/run-benchmark.sh --mode local run

# Stop benchmark runtime (workflows + worker + temporal).
# scripts/run-benchmark.sh --mode local stop
```

By default local mode uses `examples/config.local.yaml`.

### Chain Selection From jsonnet

`evm-benchmark` loads chain runtime settings from local `config/chains.jsonnet`.

Set one of:

```bash
export CHAIN_CONFIG=evmd
export CHAINS_CONFIG_PATH=./config/chains.jsonnet
```

or set `benchmark.chain_config` and `benchmark.chains_config_path` in `examples/config.yaml`.

When enabled, these values are sourced from jsonnet and applied automatically:

- `binary` (`cmd`)
- `chain_id`
- `address_prefix` (`account-prefix`)
- `denom` (`evm_denom`)
- `evm_chain_id`

## Notes

- `scripts/run-benchmark.sh build-evmd` compiles `evmd` in Docker via `docker/evmd.Dockerfile`.
- `scripts/run-benchmark.sh run` first performs runtime cleanup, then prefixes logs as `[temporal]`, `[worker]`, and `[starter]`.
- `scripts/run-benchmark.sh cleanup-workflows` terminates known benchmark workflow IDs (`evm-benchmark-sample`, `evm-benchmark-local`, and the configured `start.workflow_id`).
- `scripts/run-benchmark.sh stop` performs runtime cleanup (terminate workflows, stop worker, stop Temporal, and remove `evm-benchmark-*` containers in docker mode).
- With `skip_generate_layout: true`, workflow reuses existing `benchmark.data_dir/nodes.json` and node homes.
- In docker mode startup failures, logs are written to `benchmark.out_dir/node_<n>_startup-rpc.log` (or `benchmark.data_dir/docker-logs/...` if `out_dir` is empty).
