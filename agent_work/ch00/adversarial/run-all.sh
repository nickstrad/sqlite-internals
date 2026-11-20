#!/usr/bin/env bash
# Runs every Chunk 0 adversarial attack group in order from the repository root
# and writes the combined report to agent_work/ch00/adversarial/results.txt.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

RESULTS="agent_work/ch00/adversarial/results.txt"
mkdir -p "$(dirname "$RESULTS")"

ATTACKS=(
  pin-check
  course-checks
  trace-parser
  kill-at-syscall
  sqlitex-settings
)

{
  echo "# Chunk 0 adversarial test run"
  echo "# repo: $REPO_ROOT"
  echo "# host: $(uname -s)/$(uname -m) go=$(go version | awk '{print $3}')"
  echo "# scale: KILL-04 uses 5 runs x 3 kill points (15 strace runs); SET-01/SET-02 open 50 concurrent pooled connections"
  echo ""
  for name in "${ATTACKS[@]}"; do
    echo "## group: $name"
    go run "./agent_work/ch00/adversarial/$name"
    echo ""
  done
} | tee "$RESULTS"
