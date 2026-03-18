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
- `examples/config.local.yaml`: runnable config sample.

## Steps

### 1. Prepare Patched Image

Run this in `evm-benchmark`:

```bash
export CHAIN_CONFIG=evmd
export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"

scripts/build-evmd-testground.sh --tag evmd-testground:latest

go run ./cmd/benchctl build-image \
	--dockerfile ./docker/base.Dockerfile \
	--context . \
	--build-args BASE_IMAGE=evmd-testground:latest \
	--tag evmd-benchmark-base:local

rm -rf /tmp/data && go run ./cmd/benchctl gen \
	--config ./examples/config.local.yaml \
	--data-root /private/tmp/data \
	--clean

go run ./cmd/benchctl patchimage \
	--config ./examples/config.local.yaml \
	--from-image evmd-benchmark-base:local \
	--to-image evmd-benchmark-patched:local \
	--source-dir /private/tmp/data/out \
	--dst /data
```

### 2. Run Benchmark With Temporal

1. Ensure `examples/config.local.yaml` points to the patched image:

```yaml
benchmark:
	runner_type: docker
	start_node: true
	docker_image: "evmd-benchmark-patched:local"
	patch_image:
		enabled: false
	skip_generate_layout: true
```

`skip_generate_layout: true` tells the workflow to reuse `benchmark.data_dir/nodes.json`
and existing node home folders. This avoids rerunning bootstrap and rewriting
`config/genesis.json` on each `starter` run.

2. Start Temporal server:

```bash
temporal server start-dev --ip 127.0.0.1 --port 7233
```

3. Start worker:

```bash
go run ./cmd/worker -config ./examples/config.local.yaml
```

4. Start workflow:

```bash
go run ./cmd/starter -config ./examples/config.local.yaml
```

5. Check results in `benchmark.out_dir` (default `/tmp/evm-benchmark/output`).

### Chain Selection From jsonnet

`evm-benchmark` loads chain runtime settings from local `config/chains.jsonnet`.

Set one of:

```bash
export CHAIN_CONFIG=evmd
export CHAINS_CONFIG_PATH=./config/chains.jsonnet
```

or set `benchmark.chain_config` and `benchmark.chains_config_path` in `examples/config.local.yaml`.

When enabled, these values are sourced from jsonnet and applied automatically:

- `binary` (`cmd`)
- `chain_id`
- `address_prefix` (`account-prefix`)
- `denom` (`evm_denom`)
- `evm_chain_id`

### `patchimage`

The workflow can perform a patch step directly in Go.

Docker mode:

- build temp Dockerfile with `FROM <fromimage>` and `ADD ./out <dst>`
- tag to `<toimage>`
- use the patched image for docker node startup

Local mode:

- copy `patch_image.source_dir` into `patch_image.dest`
- if `patch_image.dest` is empty (or `/data`), `benchmark.data_dir` is used
- used to mimic the same prepared-layout flow without Docker

Config fields:

```yaml
benchmark:
	runner_type: docker # or local
	start_node: true
	patch_image:
		enabled: true
		from_image: your-base-image:tag   # docker-only; optional; defaults to docker_image
		to_image: your-patched-image:tag  # docker-only; optional; defaults to <from>-patched
		source_dir: /private/tmp/data/out # optional; defaults to <data_dir>/out or data_dir
		dest: /data                       # docker default: /data; local default: data_dir
```
