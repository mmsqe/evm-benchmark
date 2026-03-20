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
