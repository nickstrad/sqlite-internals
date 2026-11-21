#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

OUT="agent_work/ch04/adversarial/results.txt"
ATTACK_GROUPS=(
  "journal-parser"
  "rollback-crashes"
  "cache-limits"
  "cache-scale"
)

run_suite() {
  local suite_start failed=0
  suite_start=$(date +%s)
  echo "# Chapter 4 adversarial suite (Gemini 4 Neon)"
  echo "# Started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# Scratch: _work/adv-ch04/"
  echo

  for group in "${ATTACK_GROUPS[@]}"; do
    local t0 t1 rc=0
    t0=$(date +%s)
    echo "## group: $group"
    if go run "./agent_work/ch04/adversarial/$group"; then
      rc=0
    else
      rc=$?
      failed=1
    fi
    t1=$(date +%s)
    echo "# $group finished in $((t1 - t0))s (rc=$rc)"
    echo
  done

  local suite_end
  suite_end=$(date +%s)
  echo "# Total elapsed: $((suite_end - suite_start))s"
  return "$failed"
}

run_suite | tee "$OUT"
