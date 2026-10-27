#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Simplified benchmark workflow helper.

Usage:
  scripts/run-benchmark.sh [options] <command>

Commands:
  clean      Remove generated benchmark data/output paths
  cleanup-workflows Terminate known benchmark workflows in Temporal
  stop       Stop benchmark runtime (workflows, worker, temporal, containers)
  build-evmd Build local evmd-testground image in Docker
  prepare    Clean + prepare layout/images
  temporal   Start local Temporal dev server
  worker     Start benchmark worker
  starter    Start benchmark workflow
  run        Stop runtime + prepare + start temporal + run worker/starter

Options:
  --mode <docker|local|tempo|tempo-docker|tempo-remote|allegro|reth>
                          Runner mode (default: docker). "tempo" benchmarks a
                          Tempo devnet locally; "tempo-docker" runs the Tempo
                          validators as containers via docker compose (needs
                          tempo-xtask on the host and a tempo image);
                          "tempo-remote" sends to a running Tempo network over
                          its public RPC (Moderato by default; needs
                          TEMPO_BENCH_MNEMONIC, no tempo binaries);
                          "allegro" benchmarks an Allegro devnet locally (needs
                          allegro and allegro-xtask); "reth" benchmarks a reth
                          dev producer locally (needs reth).
  --config <path>         Config file path
  --data-root <path>      Root path used by benchctl gen
  --chain-config <name>   CHAIN_CONFIG value (default: evmd)

Docker-mode prepare options:
  --tag <image:tag>       evmd testground image tag (default: evmd-testground:latest)
  --commit-sha <ref>      cosmos/evm ref for evmd build (default: main)

Examples:
  scripts/run-benchmark.sh prepare
  scripts/run-benchmark.sh temporal
  scripts/run-benchmark.sh worker
  scripts/run-benchmark.sh starter

  scripts/run-benchmark.sh --mode local prepare
  scripts/run-benchmark.sh --mode local worker
  scripts/run-benchmark.sh --mode local starter

  scripts/run-benchmark.sh --mode tempo run
  scripts/run-benchmark.sh --mode tempo-docker run
  TEMPO_BENCH_MNEMONIC="..." scripts/run-benchmark.sh --mode tempo-remote run

  scripts/run-benchmark.sh --mode allegro run
  scripts/run-benchmark.sh --mode reth run
  scripts/run-benchmark.sh --mode reth --config examples/config.reth.validation.yaml run
USAGE
}

MODE="docker"
CONFIG=""
DATA_ROOT=""
CHAIN_CONFIG="evmd"
TAG="evmd-testground:latest"
COMMIT_SHA="main"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --mode)
      [[ $# -ge 2 ]] || { echo "Missing value for --mode"; exit 1; }
      MODE="$2"
      shift 2
      ;;
    --config)
      [[ $# -ge 2 ]] || { echo "Missing value for --config"; exit 1; }
      CONFIG="$2"
      shift 2
      ;;
    --data-root)
      [[ $# -ge 2 ]] || { echo "Missing value for --data-root"; exit 1; }
      DATA_ROOT="$2"
      shift 2
      ;;
    --chain-config)
      [[ $# -ge 2 ]] || { echo "Missing value for --chain-config"; exit 1; }
      CHAIN_CONFIG="$2"
      shift 2
      ;;
    --tag)
      [[ $# -ge 2 ]] || { echo "Missing value for --tag"; exit 1; }
      TAG="$2"
      shift 2
      ;;
    --commit-sha)
      [[ $# -ge 2 ]] || { echo "Missing value for --commit-sha"; exit 1; }
      COMMIT_SHA="$2"
      shift 2
      ;;
    clean|cleanup-workflows|stop|build-evmd|prepare|temporal|worker|starter|run)
      COMMAND="$1"
      shift
      break
      ;;
    *)
      echo "Unknown argument: $1"
      usage
      exit 1
      ;;
  esac
done

if [[ -z "${COMMAND:-}" ]]; then
  echo "Missing command"
  usage
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

case "$MODE" in
  docker|local|tempo|tempo-docker|tempo-remote|allegro|reth) ;;
  *)
    echo "Invalid --mode: $MODE (expected docker, local, tempo, tempo-docker, tempo-remote, allegro or reth)"
    exit 1
    ;;
esac

if [[ -z "$CONFIG" ]]; then
  case "$MODE" in
    docker) CONFIG="./examples/config.yaml" ;;
    tempo)  CONFIG="./examples/config.tempo.yaml" ;;
    tempo-docker) CONFIG="./examples/config.tempo.docker.yaml" ;;
    tempo-remote) CONFIG="./examples/config.tempo.moderato.yaml" ;;
    allegro) CONFIG="./examples/config.allegro.yaml" ;;
    reth)   CONFIG="./examples/config.reth.yaml" ;;
    *)      CONFIG="./examples/config.local.yaml" ;;
  esac
fi

if [[ -z "$DATA_ROOT" ]]; then
  case "$MODE" in
    docker) DATA_ROOT="/private/tmp/data" ;;
    tempo)  DATA_ROOT="/tmp/tempo-benchmark/data" ;;
    # Under $HOME so colima can bind-mount each node dir into its container.
    tempo-docker) DATA_ROOT="$HOME/tempo-benchmark-docker/data" ;;
    tempo-remote) DATA_ROOT="/tmp/tempo-remote-benchmark/data" ;;
    allegro) DATA_ROOT="/tmp/allegro-benchmark/data" ;;
    reth)   DATA_ROOT="/tmp/reth-benchmark/data" ;;
    *)      DATA_ROOT="/private/tmp/evm-benchmark-local/data" ;;
  esac
fi

# Chain profiles are cosmos-only; exporting one would override the tempo /
# allegro / reth config.
if [[ "$MODE" == tempo* || "$MODE" == allegro || "$MODE" == reth ]]; then
  unset CHAIN_CONFIG
else
  export CHAIN_CONFIG
fi

RUN_TEMPORAL_PID=""
RUN_WORKER_PID=""
RUN_STARTER_PID=""
LAST_LAUNCHED_PID=""
# The worker and starter run as binaries built here rather than via `go run`,
# which dies on TERM and leaves the program it started running.
BIN_DIR="${TMPDIR:-/tmp}/evm-benchmark-bin"

log_step() {
  local msg="$1"
  printf '[run-benchmark] %s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$msg"
}

# Runs a command in the background with its output prefixed and records its PID
# in LAST_LAUNCHED_PID. No shell sits between the two, so a signal to that PID
# reaches the command itself.
start_prefixed() {
  local prefix="$1"
  shift
  "$@" > >(sed -u "s/^/[$prefix] /") 2>&1 &
  LAST_LAUNCHED_PID="$!"
}

# Sends TERM to the given PIDs, then KILL to any still running after the grace
# period.
terminate_pids() {
  local grace_seconds="$1"
  shift
  (( $# > 0 )) || return 0

  kill -TERM "$@" 2>/dev/null || true
  local deadline=$((SECONDS + grace_seconds)) pid running
  while (( SECONDS < deadline )); do
    running=0
    for pid in "$@"; do
      if kill -0 "$pid" 2>/dev/null; then
        running=1
        break
      fi
    done
    (( running )) || return 0
    sleep 0.2
  done
  kill -KILL "$@" 2>/dev/null || true
}

terminate_matching_processes() {
  local pids=()
  mapfile -t pids < <(pgrep -f "$1" || true)
  (( ${#pids[@]} > 0 )) || return 0
  terminate_pids "${2:-5}" "${pids[@]}"
}

wait_for_tcp_port() {
  local host="$1"
  local port="$2"
  local timeout_seconds="${3:-30}"
  local deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    if (echo >"/dev/tcp/$host/$port") >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done

  return 1
}

run_clean() {
  log_step "clean: removing temporary benchmark directories"
  rm -rf /private/tmp/data /private/tmp/evm-benchmark-local /private/tmp/evm-benchmark
  case "$MODE" in
    tempo) rm -rf /tmp/tempo-benchmark ;;
    tempo-docker) rm -rf "$HOME/tempo-benchmark-docker" ;;
    tempo-remote) rm -rf /tmp/tempo-remote-benchmark ;;
    allegro) rm -rf /tmp/allegro-benchmark ;;
    reth) rm -rf /tmp/reth-benchmark ;;
  esac
}

run_gen() {
  log_step "prepare: generating layout via benchctl gen"
  go run ./cmd/benchctl gen \
    --config "$CONFIG" \
    --data-root "$DATA_ROOT" \
    --clean
  log_step "prepare: benchctl gen completed"
}

run_worker() {
  log_step "worker: starting foreground worker"
  go run ./cmd/worker -config "$CONFIG"
}

run_starter() {
  log_step "starter: starting foreground workflow starter"
  go run ./cmd/starter -config "$CONFIG"
}

cleanup_benchmark_workflows() {
  if ! command -v temporal >/dev/null 2>&1; then
    return
  fi

  local cfg_workflow_id=""
  cfg_workflow_id="$(awk '/^[[:space:]]*workflow_id:[[:space:]]*/{print $2; exit}' "$CONFIG" | tr -d '"' || true)"

  local workflow_ids=()
  if [[ -n "$cfg_workflow_id" ]]; then
    workflow_ids+=("$cfg_workflow_id")
  fi
  workflow_ids+=("evm-benchmark-sample" "evm-benchmark-local")

  local -A seen=()
  local workflow_id=""
  for workflow_id in "${workflow_ids[@]}"; do
    if [[ -z "$workflow_id" ]]; then
      continue
    fi
    if [[ -n "${seen[$workflow_id]:-}" ]]; then
      continue
    fi
    seen[$workflow_id]=1

    temporal workflow terminate \
      --namespace default \
      --workflow-id "$workflow_id" \
      --reason "evm-benchmark cleanup" \
      >/dev/null 2>&1 || true
  done
}

stop_runtime() {
  log_step "stop: terminating workflows and runtime processes"
  cleanup_benchmark_workflows

  # Stop workers first so gRPC polls exit cleanly before Temporal shuts down.
  # The pattern matches the built binary and a `go run ./cmd/worker` alike.
  terminate_matching_processes "/worker -config" 8
  terminate_matching_processes "temporal server start-dev" 5
  stop_nodes
  log_step "stop: runtime cleanup complete"
}

# Stops the nodes a run in this mode started.
stop_nodes() {
  case "$MODE" in
    docker) cleanup_benchmark_containers ;;
    # Tempo nodes are launched from the generated per-node run.sh wrapper.
    tempo) terminate_matching_processes "tempo-benchmark/data/devnet" 8 ;;
    tempo-docker) cleanup_tempo_compose ;;
    # The allegro launcher cd's into the node home and passes relative paths,
    # so the data dir never reaches the command line; match the launched
    # command instead.
    allegro) terminate_matching_processes "allegro node --chain" 8 ;;
    # Same for reth; a fullnode's launcher may still be waiting for the
    # producer, so stop those shells too.
    reth)
      terminate_matching_processes "reth node --chain ./genesis.json --datadir ." 8
      terminate_matching_processes "reth-benchmark/data/devnet/node[0-9]*/run.sh" 2
      ;;
  esac
}

# Tear down the compose-managed Tempo devnet by project label. Not by
# `compose -f`: the compose file lives under the run's data_dir, which differs
# between `benchctl gen` (data/out/devnet) and `run` (data/devnet), and
# `compose -f <missing>` errors out instead of resolving the project by name.
cleanup_tempo_compose() {
  if ! command -v docker >/dev/null 2>&1; then
    return
  fi
  local project="${TEMPO_COMPOSE_PROJECT:-evm-benchmark-tempo}"
  local ids
  ids="$(docker ps -aq --filter "label=com.docker.compose.project=$project" || true)"
  if [[ -n "$ids" ]]; then
    docker rm -f $ids >/dev/null 2>&1 || true
  fi
}

cleanup_benchmark_containers() {
  if ! command -v docker >/dev/null 2>&1; then
    return
  fi

  # Remove stale benchmark containers to avoid docker name conflicts on rerun.
  local ids
  ids="$(docker ps -aq --filter name='^evm-benchmark-' || true)"
  if [[ -n "$ids" ]]; then
    docker rm -f $ids >/dev/null 2>&1 || true
  fi
}

# Stops what run_all started. Nodes too: a worker stopped before its activities
# finish leaves them running. Docker containers are kept for `docker logs`; the
# next run or `stop` removes them.
cleanup_run_all() {
  # shellcheck disable=SC2086 # unset PIDs drop out
  terminate_pids 8 $RUN_STARTER_PID $RUN_WORKER_PID $RUN_TEMPORAL_PID
  [[ "$MODE" == docker ]] || stop_nodes
}

on_signal() {
  log_step "run: SIG$1 received, stopping"
  trap - EXIT INT TERM
  cleanup_run_all
  exit $((128 + $(kill -l "$1")))
}

run_all() {
  trap cleanup_run_all EXIT
  trap 'on_signal INT' INT
  trap 'on_signal TERM' TERM

  log_step "run: mode=$MODE config=$CONFIG data_root=$DATA_ROOT"
  log_step "run: step 1/6 stop existing runtime"

  stop_runtime

  log_step "run: step 2/6 prepare benchmark artifacts"
  run_prepare
  log_step "run: prepare completed"

  log_step "run: step 3/6 build worker and starter"
  mkdir -p "$BIN_DIR"
  go build -o "$BIN_DIR/" ./cmd/worker ./cmd/starter

  log_step "run: step 4/6 start temporal dev server"
  start_prefixed "temporal" temporal server start-dev --ip 127.0.0.1 --port 7233
  RUN_TEMPORAL_PID="$LAST_LAUNCHED_PID"
  log_step "run: temporal pid=$RUN_TEMPORAL_PID; waiting for 127.0.0.1:7233"
  if ! wait_for_tcp_port "127.0.0.1" "7233" 40; then
    log_step "run: temporal readiness timed out"
    echo "Temporal did not become ready on 127.0.0.1:7233"
    return 1
  fi
  log_step "run: temporal is ready"

  log_step "run: step 5/6 start worker"
  start_prefixed "worker" "$BIN_DIR/worker" -config "$CONFIG"
  RUN_WORKER_PID="$LAST_LAUNCHED_PID"
  log_step "run: worker pid=$RUN_WORKER_PID"
  sleep 2

  log_step "run: step 6/6 start workflow starter"
  # In the background, so a signal is handled at once instead of after the
  # starter exits. Its status becomes the run's.
  start_prefixed "starter" "$BIN_DIR/starter" -config "$CONFIG"
  RUN_STARTER_PID="$LAST_LAUNCHED_PID"
  wait "$RUN_STARTER_PID"
}

run_build_evmd() {
  log_step "docker: building evmd image tag=$TAG commit=$COMMIT_SHA"
  DOCKER_BUILDKIT=1 docker build \
    -f ./docker/evmd.Dockerfile \
    --build-arg COMMIT_SHA="$COMMIT_SHA" \
    -t "$TAG" \
    .

  echo "Built image: $TAG"
  docker image inspect "$TAG" --format '{{.RepoTags}}'
  log_step "docker: evmd image build complete"
}

run_prepare_docker() {
  log_step "prepare(docker): clean temporary data"
  run_clean

  log_step "prepare(docker): remove stale benchmark containers"
  cleanup_benchmark_containers

  log_step "prepare(docker): build evmd image"
  run_build_evmd

  log_step "prepare(docker): build benchmark base image"
  go run ./cmd/benchctl build-image \
    --dockerfile ./docker/base.Dockerfile \
    --context . \
    --build-args BASE_IMAGE="$TAG" \
    --tag evmd-benchmark-base:local

  run_gen

  log_step "prepare(docker): patch benchmark image with generated data"
  go run ./cmd/benchctl patchimage \
    --config "$CONFIG" \
    --from-image evmd-benchmark-base:local \
    --to-image evmd-benchmark-patched:local \
    --source-dir "$DATA_ROOT/out" \
    --dst /data
  log_step "prepare(docker): completed"
}

run_prepare() {
  if [[ "$MODE" == "docker" ]]; then
    run_prepare_docker
  else
    run_prepare_local
  fi
}

run_prepare_local() {
  log_step "prepare(local): clean temporary data"
  run_clean

  log_step "prepare(local): generate layout"
  run_gen
  log_step "prepare(local): completed"
}

case "$COMMAND" in
  clean)
    run_clean
    ;;
  cleanup-workflows)
    cleanup_benchmark_workflows
    ;;
  stop)
    stop_runtime
    ;;
  build-evmd)
    run_build_evmd
    ;;
  prepare)
    run_prepare
    ;;
  temporal)
    temporal server start-dev --ip 127.0.0.1 --port 7233
    ;;
  worker)
    run_worker
    ;;
  starter)
    run_starter
    ;;
  run)
    run_all
    ;;
esac
