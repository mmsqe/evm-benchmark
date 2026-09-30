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

## Measuring

Every run writes `block_stats.log` and echoes a summary line. Four lines matter:

| line | read it as |
|---|---|
| `tx_summary` | `included` far below `sent` means the run measured rejection |
| `tps_summary` | **quote `full_second`** — see below |
| `tps_warning` | fewer than 5 whole seconds; the load, not the chain, set the rate |
| `send_warning` | the pool never reached a quarter of the load, so the chain kept pace with the sender and the rate is a lower bound |

Rates are transactions per whole second of block timestamps. `full_second`
covers only seconds lying entirely inside the load; `sustained` divides by the
whole active window, including its partial first and last second, and so moves
with where the load fell against a second boundary — two runs of one binary
differed 14% on `sustained` while agreeing to 1% on `full_second`. Seconds,
rather than a sliding window of blocks, because block timestamps have only
second resolution: a 5-block window spans ~2s at 2.5 blocks/s but ~0.3s at 15,
which understates a fast chain ~3x. The two warnings are independent — a run can
span plenty of whole seconds while never making the chain work.

Three settings decide whether a number means anything. All are set in the
shipped examples:

- **`broadcast_pending_watermark`** — negative never pauses. A positive value
  caps how deep a backlog the chain is given, so a chain that drains faster than
  the sender refills ends up measuring the sender.
- **Pool capacity** — derived from `num_accounts * num_txs`, because anything
  past a sender's slot limit is *rejected*, not queued. Don't pin it in config.
- **`num_idle`** — counts *blocks* without transactions. On a chain producing
  tens of blocks per second, a cosmos-sized value ends the run mid-drain.

### Comparing a block-building change

A chain that keeps pace with the sender says nothing about how it builds blocks.
To put building on the critical path: `broadcast_batch_size: 100`, cap the block
gas limit so throughput becomes blocks/s x txs/block (50M holds ~2,380
transfers), confirm `send_warning` is absent, then run five interleaved pairs and
compare `full_second` medians.

Measured that way, a change pipelining payload building came out at +12.8%
(+10.1-14.8% across five pairs, 3% within-branch spread). Without the setup the
same change was invisible: both branches sat at the sender's ceiling, within
0.2% of each other.

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

1. `tempo` and `tempo-xtask` on your `PATH` — `cargo install` them, or build in
   the tempo repo (`cargo build --bin tempo --bin tempo-xtask`, add `--release`
   for real runs) and point the config at the build. These are the only external
   binaries: the devnet and the native `0x76` transactions are both generated
   in-process.
2. The `temporal` CLI (the run script starts a dev server itself).

### Configure

`examples/config.tempo.yaml` resolves `tempo` and `tempo-xtask` from your `PATH`
by default (`which tempo`). Override only to benchmark a specific build:

```yaml
tempo_bin:       /path/to/tempo/target/release/tempo
tempo_xtask_bin: /path/to/tempo/target/release/tempo-xtask
```

The docker profile (`examples/config.tempo.docker.yaml`) uses the in-image
`tempo` command name for `tempo_bin`.

### Run

```bash
scripts/run-benchmark.sh --mode tempo run
```

That stops any previous runtime, generates the devnet, pre-signs the
transactions, starts the node, sends the load, and writes
`/tmp/tempo-benchmark/output/node_0_block_stats.log`. Stop everything with
`scripts/run-benchmark.sh --mode tempo stop`.

The line that matters is `tx_summary`: if `included` is far below `sent`, the
run measured rejection, not throughput; `tps_summary` carries the rate. See
[Measuring](#measuring).

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

### Public testnet (Moderato)

`remote_rpc_url` sends to a Tempo network that is already running. Nothing is
generated or started, and no tempo binaries are needed:

```bash
export TEMPO_BENCH_MNEMONIC="$(cast wallet new-mnemonic | sed -n '/^Phrase:/{n;p;}')"
scripts/run-benchmark.sh --mode tempo-remote run   # examples/config.tempo.moderato.yaml
```

- Sign with your own mnemonic; the devnet one is refused, since its accounts are
  public. It still lands in `data_dir/config.json` and Temporal history, so use
  a throwaway.
- `tempo_faucet: true` funds senders that cannot cover the run's worst-case
  fees via `tempo_fundAddress`.
- Nonces are read from the chain, so accounts can be reused.
- Only this run's transactions count, by the hash the node returned (Tempo
  stores the signature's `v` as 27/28, so keccak of the bytes sent never
  matches); the run ends once they have all landed.

The public endpoint rate-limits (HTTP 429, waited out), caps batches (5
`eth_sendRawTransaction` pass, 10 do not) and hides `txpool_status`. The rate
reflects the gateway as much as the chain: use it for inclusion behaviour, not
capacity, and keep loads small.

2D nonces (`tempo_nonce_key`, `tempo_nonce_lanes`) let fewer accounts carry the
load and keep the run off each account's own nonce, at 22,100 gas for a key's
first transaction and 5,000 after.

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

## Allegro Mode

Benchmarks an [Allegro](https://github.com/yihuang/allegro) devnet — a Reth
execution node embedded in a Commonware simplex consensus engine. It is a stock
reth EVM behind consensus, so unlike Tempo nothing about the transaction path is
family-specific: ordinary legacy/London transfers from the shared signer, 21,000
gas each.

### Prerequisites

1. `allegro` and `allegro-xtask` on your `PATH` — `cargo install` them, or build
   in the allegro repo (`cargo build --bin allegro --bin allegro-xtask`, add
   `--release` for real runs) and point the config at the build. These are the
   only external binaries: the devnet is generated in-process from
   `allegro-xtask genesis`.
2. The `temporal` CLI (the run script starts a dev server itself).

### Run

```bash
scripts/run-benchmark.sh --mode allegro run
```

Generates the devnet, pre-signs the transactions, starts the node, sends the
load, and writes `/tmp/allegro-benchmark/output/node_0_block_stats.log` (see
[Measuring](#measuring)). Stop everything with
`scripts/run-benchmark.sh --mode allegro stop`.

### Configure

`examples/config.allegro.yaml` resolves both binaries from your `PATH`. Override
only to benchmark a specific build:

```yaml
allegro_bin:       /path/to/allegro/target/release/allegro
allegro_xtask_bin: /path/to/allegro/target/release/allegro-xtask
```

Ports are allocated in blocks of 4 per node (consensus p2p, execution p2p,
authrpc, http) from `allegro_base_port`, so node0 serves JSON-RPC on
`http://127.0.0.1:9003` and node1 on `9007`. Those consensus sockets are baked
into the generated genesis as the validator set, which is also how peers find
each other — there is no separate peer list.

- `allegro_gas_limit` goes into genesis *and* pins the builder target, capping a
  block at `gas_limit/21000` transfers — report it with any result. Unset keeps
  allegro-xtask's 30M default (~1,428 per block).
- `allegro_leader_timeout_ms` / `allegro_cert_timeout_ms` set the block cadence;
  unset means the binary's defaults (2000/4000).
- `allegro_node_args` is appended verbatim to every launcher, for flags the
  benchmark does not model (e.g. `--builder.interval 200ms`).

### Funding

`allegro-xtask` prefunds 20 anvil accounts on HD branch 0, while the generator
signs from `m/44'/60'/{node}'/0/{1..num_accounts}`, so the benchmark adds every
account it will sign from to the genesis `alloc` — without that only node 0's
first 19 transactions could pay. Verify against a running node (allegro has no
fee token, hence the empty flag):

```bash
go run ./cmd/checkfunding -rpc http://127.0.0.1:9003 -chain-id 1337 -fee-token ""
```

### Constraints

- Allegro produces tens of blocks per second, so `num_idle` (a count of blocks)
  must stay high; the example uses 40. Its transaction pool is sized from the
  spec automatically, so it needs no `--txpool.*` flags of its own.
- `runner_type: docker` is rejected — there is no allegro image, and the cosmos
  docker runner is cosmos-shaped.
- `fullnodes` must be 0: every node in an allegro genesis validator set votes.
- `tx_type: erc20-transfer` is rejected unless `erc20_contract_address` is set;
  allegro genesis deploys no contracts, so a transfer to an empty account would
  succeed while executing nothing.

### Tests

```bash
ALLEGRO_XTASK_BIN=/path/to/allegro-xtask go test ./internal/activities -run Allegro
```

The devnet-bootstrapping test skips unless that is set; the rest (launcher
format, pool sizing, funding-vs-signer agreement) runs unconditionally.

## Comparing chains

Run the three examples one at a time on the same host:

```bash
scripts/run-benchmark.sh --mode local   run   # evmd
scripts/run-benchmark.sh --mode tempo   run   # tempo
scripts/run-benchmark.sh --mode allegro run   # allegro
```

They hold everything that drives a chain identical — concurrency 32, batch size
100, a never-pausing watermark, derived pool capacity, a non-binding 3e9 block
gas limit, one validator, `num_idle: 40`. The **load is not shared**:
`full_second` is a rate, so sizes may differ as long as each run is saturated
and spans five whole seconds, and no single size manages that everywhere.

| chain | load | full_second | peak backlog | saturated? |
|---|---|---|---|---|
| allegro | 500,000 | 78,292 | 8,360 (1.7%) | no — lower bound |
| tempo | 500,000 | 55,248 | 31,581 (6.3%) | no — lower bound |
| evmd | 200,000 | **11,426** | 76,466 (38%) | yes |

Only evmd was made to work at capacity; the other two drained the load as fast
as it arrived and carry `send_warning`, so their figures mean "at least this
fast". Raising the load alone will not fix that — the sender has to outrun the
chain, which for these two means constraining block production.

Two things to know before changing the loads:

- **evmd wants ~100 transactions per sender, and few senders.** Deeper queues
  collapse it (2,000 x 160 included 2,636 of 320,000; 2,000 x 250 never
  finished) but so does spreading the same load thinner: 8,000 x 40 measured
  2,127 tx/s against 6,695 for 2,000 x 100 in docker, because more senders make
  its proposal assembly walk more accounts and blocks came out at 103 rather
  than ~700. Shrinking the load costs accuracy too — 2,000 x 50 gave 7,921
  against 11,426, the backlog never deepening.
- **Docker costs about 1.7x** on evmd: 6,695 against 11,426 for the identical
  2,000 x 100 shape.
- **Not aligned, and worth stating with any number:** transaction shape (evmd
  and allegro run the same 21,000-gas native transfer; tempo rejects native
  transfers and runs a ~271,000-gas TIP-20 one) and block cadence, which is each
  chain's own consensus default. The allegro row is its `main` branch.

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
