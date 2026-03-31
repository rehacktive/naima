#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

cd "${REPO_ROOT}"

usage() {
  cat <<'EOF'
Usage:
  ./scripts/rebuild_mismatched_pkb_embeddings.sh [--apply] [--restart]

Behavior:
  - loads .env through the Go maintenance command
  - detects PKB and memory embeddings whose dimensions do not match NAIMA_PGVECTOR_EMBEDDING_DIMS
  - dry-runs by default
  - with --apply, regenerates those embeddings using the current OPENAI_EMBEDDING_MODEL
  - with --restart, restarts the production stack afterward
EOF
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

APPLY=0
RESTART=0

for arg in "$@"; do
  case "$arg" in
    --apply)
      APPLY=1
      ;;
    --restart)
      RESTART=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $arg" >&2
      usage >&2
      exit 1
      ;;
  esac
done

require_cmd go

CMD=(go run ./cmd/reembed_embeddings)
if [[ "${APPLY}" -eq 1 ]]; then
  CMD+=(--apply)
fi

"${CMD[@]}"

if [[ "${RESTART}" -eq 1 ]]; then
  require_cmd docker
  if ! docker compose version >/dev/null 2>&1; then
    echo "docker compose plugin is required for --restart" >&2
    exit 1
  fi
  echo "Restarting production stack..."
  docker compose up -d --build
fi
