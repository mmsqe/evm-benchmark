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
  --mode <docker|local>   Runner mode (default: docker)
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
  docker|local) ;;
  *)
    echo "Invalid --mode: $MODE (expected docker or local)"
    exit 1
    ;;
esac

if [[ -z "$CONFIG" ]]; then
  if [[ "$MODE" == "docker" ]]; then
    CONFIG="./examples/config.yaml"
  else
    CONFIG="./examples/config.local.yaml"
  fi
fi

if [[ -z "$DATA_ROOT" ]]; then
  if [[ "$MODE" == "docker" ]]; then
    DATA_ROOT="/private/tmp/data"
  else
    DATA_ROOT="/private/tmp/evm-benchmark-local/data"
  fi
fi

export CHAIN_CONFIG

RUN_WORKER_PID=""
RUN_TEMPORAL_PID=""
LAST_LAUNCHED_PID=""

log_step() {
  local msg="$1"
  printf '[run-benchmark] %s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$msg"
}

start_prefixed_group() {
  local prefix="$1"
  shift

  # Start each long-running command in its own process group so cleanup can
  # terminate the full pipeline instead of only the parent shell.
  if command -v setsid >/dev/null 2>&1; then
    setsid bash -c "$* 2>&1 | sed -u 's/^/[$prefix] /'" &
  else
    bash -c "$* 2>&1 | sed -u 's/^/[$prefix] /'" &
  fi
  LAST_LAUNCHED_PID="$!"
}

terminate_matching_processes() {
  local pattern="$1"
  local grace_seconds="${2:-5}"
  local deadline=$((SECONDS + grace_seconds))
  local pids=()

  mapfile -t pids < <(pgrep -f "$pattern" || true)
  if [[ ${#pids[@]} -eq 0 ]]; then
    return
  fi

  kill -TERM "${pids[@]}" >/dev/null 2>&1 || true

  while (( SECONDS < deadline )); do
    local still_running=0
    local pid
    for pid in "${pids[@]}"; do
      if kill -0 "$pid" 2>/dev/null; then
        still_running=1
        break
      fi
    done

    if (( still_running == 0 )); then
      return
    fi

    sleep 0.2
  done

  kill -KILL "${pids[@]}" >/dev/null 2>&1 || true
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
  terminate_matching_processes "go run ./cmd/worker -config" 8
  terminate_matching_processes "cmd/worker -config" 8
  terminate_matching_processes "/worker -config" 8
  terminate_matching_processes "/exe/worker -config" 8
  terminate_matching_processes "temporal server start-dev" 5

  if [[ "$MODE" == "docker" ]]; then
    cleanup_benchmark_containers
  fi
  log_step "stop: runtime cleanup complete"
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

cleanup_run_all() {
  if [[ -n "${RUN_WORKER_PID:-}" ]] && kill -0 "$RUN_WORKER_PID" 2>/dev/null; then
    kill -TERM "-$RUN_WORKER_PID" 2>/dev/null || true
    kill -TERM "$RUN_WORKER_PID" 2>/dev/null || true
    wait "$RUN_WORKER_PID" 2>/dev/null || true
  fi

  if [[ -n "${RUN_TEMPORAL_PID:-}" ]] && kill -0 "$RUN_TEMPORAL_PID" 2>/dev/null; then
    kill -TERM "-$RUN_TEMPORAL_PID" 2>/dev/null || true
    kill -TERM "$RUN_TEMPORAL_PID" 2>/dev/null || true
    wait "$RUN_TEMPORAL_PID" 2>/dev/null || true
  fi
}

run_all() {
  trap cleanup_run_all EXIT INT TERM

  log_step "run: mode=$MODE config=$CONFIG data_root=$DATA_ROOT"
  log_step "run: step 1/5 stop existing runtime"

  stop_runtime

  log_step "run: step 2/5 prepare benchmark artifacts"
  if [[ "$MODE" == "docker" ]]; then
    run_prepare_docker
  else
    run_prepare_local
  fi
  log_step "run: prepare completed"

  log_step "run: step 3/5 start temporal dev server"
  start_prefixed_group "temporal" "temporal server start-dev --ip 127.0.0.1 --port 7233"
  RUN_TEMPORAL_PID="$LAST_LAUNCHED_PID"
  log_step "run: temporal pid=$RUN_TEMPORAL_PID; waiting for 127.0.0.1:7233"
  if ! wait_for_tcp_port "127.0.0.1" "7233" 40; then
    log_step "run: temporal readiness timed out"
    echo "Temporal did not become ready on 127.0.0.1:7233"
    return 1
  fi
  log_step "run: temporal is ready"

  log_step "run: step 4/5 start worker"
  start_prefixed_group "worker" "go run ./cmd/worker -config '$CONFIG'"
  RUN_WORKER_PID="$LAST_LAUNCHED_PID"
  log_step "run: worker pid=$RUN_WORKER_PID"
  sleep 2

  log_step "run: step 5/5 start workflow starter"
  go run ./cmd/starter -config "$CONFIG" 2>&1 | sed -u 's/^/[starter] /'
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
    if [[ "$MODE" == "docker" ]]; then
      run_prepare_docker
    else
      run_prepare_local
    fi
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
