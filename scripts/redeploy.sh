#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

cd "${REPO_ROOT}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

require_cmd git
require_cmd docker

if ! docker compose version >/dev/null 2>&1; then
  echo "docker compose plugin is required" >&2
  exit 1
fi

if [[ ! -f ".env" ]]; then
  echo ".env not found in ${REPO_ROOT}" >&2
  exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
  echo "repo has uncommitted changes; refusing to pull" >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "${branch}" == "HEAD" ]]; then
  echo "detached HEAD; refusing to pull without an active branch" >&2
  exit 1
fi

echo "Pulling latest changes for branch ${branch}..."
git pull --ff-only

echo "Stopping production stack..."
docker compose down

echo "Rebuilding and starting production stack..."
docker compose up -d --build

echo "Production stack restarted successfully."
