#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Build local evmd-testground image from the local evmd binary.

Usage:
  scripts/build-evmd-testground.sh [--tag <image:tag>] [--binary <path-to-evmd>]

Options:
  --tag <image:tag>   Docker image tag (default: evmd-testground:latest)
  --binary <path>     Path to evmd binary (default: auto-detect with `command -v evmd`)
USAGE
}

TAG="evmd-testground:latest"
BINARY_PATH=""

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
    --binary)
      [[ $# -ge 2 ]] || { echo "Missing value for --binary"; exit 1; }
      BINARY_PATH="$2"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1"
      usage
      exit 1
      ;;
  esac
done

if [[ -z "$BINARY_PATH" ]]; then
  BINARY_PATH="$(command -v evmd || true)"
fi

if [[ -z "$BINARY_PATH" ]]; then
  echo "evmd binary not found. Install it or pass --binary <path>."
  exit 1
fi

if [[ ! -x "$BINARY_PATH" ]]; then
  echo "evmd binary is not executable: $BINARY_PATH"
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

cp "$BINARY_PATH" "$TMP_DIR/evmd"
chmod +x "$TMP_DIR/evmd"

cat > "$TMP_DIR/Dockerfile" <<'EOF'
FROM ubuntu:22.04
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/*
COPY evmd /bin/evmd
ENTRYPOINT ["/bin/evmd"]
EOF

cd "$ROOT_DIR"
docker build -t "$TAG" "$TMP_DIR"

echo "Built image: $TAG"
docker image inspect "$TAG" --format '{{.RepoTags}}'
