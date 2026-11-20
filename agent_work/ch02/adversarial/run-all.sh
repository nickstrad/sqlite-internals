#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

OUT="agent_work/ch02/adversarial/results.txt"
ATTACK_GROUPS=(
  "header-falsify"
  "census-edge"
  "corrupt-fuzz"
  "manifest-capture"
)

run_suite() {
  local suite_start
  suite_start=$(date +%s)
  echo "# Chapter 2 adversarial suite (Gemini 4 Neon)"
  echo "# Started: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# Scratch: _work/adv-ch02/"
  echo

  for group in "${ATTACK_GROUPS[@]}"; do
    local t0 t1
    t0=$(date +%s)
    echo "## group: $group"
    go run "./agent_work/ch02/adversarial/$group"
    t1=$(date +%s)
    echo "# $group finished in $((t1 - t0))s"
    echo
  done

  local suite_end
  suite_end=$(date +%s)
  echo "# Total elapsed: $((suite_end - suite_start))s"
}

run_suite | tee "$OUT"
