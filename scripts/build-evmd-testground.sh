#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Build local evmd-testground image by compiling evmd inside Docker.

Usage:
  scripts/build-evmd-testground.sh [--tag <image:tag>] [--commit-sha <git-sha-or-ref>]

Options:
  --tag <image:tag>          Docker image tag (default: evmd-testground:latest)
  --commit-sha <git-sha>     cosmos/evm commit SHA or ref (default: main)
USAGE
}

TAG="evmd-testground:latest"
COMMIT_SHA="main"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
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
    *)
      echo "Unknown argument: $1"
      usage
      exit 1
      ;;
  esac
done

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
docker build \
  -f ./docker/evmd.Dockerfile \
  --build-arg COMMIT_SHA="$COMMIT_SHA" \
  -t "$TAG" \
  .

echo "Built image: $TAG"
docker image inspect "$TAG" --format '{{.RepoTags}}'
