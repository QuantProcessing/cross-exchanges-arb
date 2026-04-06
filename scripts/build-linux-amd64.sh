#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DIST_DIR="${REPO_ROOT}/dist"
OUTPUT_PATH="${DIST_DIR}/cross-arb-linux-amd64"

mkdir -p "${DIST_DIR}"

cd "${REPO_ROOT}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "${OUTPUT_PATH}" .

printf 'built %s\n' "${OUTPUT_PATH}"
