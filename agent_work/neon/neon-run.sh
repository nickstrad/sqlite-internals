#!/bin/bash
# Run Gemini 4 Neon as an agent (OpenCode) inside a directory with a brief file as the prompt.
# Usage: neon-run.sh <dir> <brief-file>
# PRIMARY route: OpenRouter (openrouter/google/gemini-4-neon), used while its key has budget.
# FALLBACK: direct Gemini API (google/gemini-4-neon) when the primary has under $1.50 left,
# exits non-zero, or ends without printing the NEON-DONE marker every brief asks for (a run
# cut off by an exhausted budget exits 0 silently). The fallback run is told the brief may be
# partly done. A fallback failure (e.g. 429) exits 75: reported, not a pipeline bug.
dir=$1; brief=$2
set -a; source /root/Software/.secrets/yt-transcribe.env; source /root/Software/.secrets/gemini-neon.env; set +a
cd "$dir" || exit 98
prompt="Read the brief file at $brief and carry it out completely in this directory ($dir). The brief is your full instruction."
resume=" NOTE: an earlier run of this same brief was cut off part-way. Inspect the working tree (git status, git diff) first, keep the finished parts, and complete everything the brief asks that is still missing."
[ -n "$NEON_RESUME" ] && prompt="$prompt$resume"
try() { # $1 model, $2 prompt
  echo "=== neon-run route=$1 $(date -Is) ==="
  timeout 5400 opencode run --auto -m "$1" "$2" 2>&1 | tee /tmp/neon-run.$$.out
  rc=${PIPESTATUS[0]}
  grep -q "NEON-DONE" /tmp/neon-run.$$.out || { [ $rc -eq 0 ] && rc=70; }
  rm -f /tmp/neon-run.$$.out
  return $rc
}
left=$(curl -s -m 20 https://openrouter.ai/api/v1/key -H "Authorization: Bearer $OPENROUTER_API_KEY" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['limit_remaining'])" 2>/dev/null || echo 0)
rc=1
if python3 -c "import sys; sys.exit(0 if float('$left' or 0) >= 1.5 else 1)"; then
  try openrouter/google/gemini-4-neon "$prompt"; rc=$?
  [ $rc -ne 0 ] && { echo "=== primary ended rc=$rc (no NEON-DONE or error); trying fallback ==="; prompt="$prompt$resume"; }
else
  echo "=== primary skipped: OpenRouter budget left \$$left ==="
fi
if [ $rc -ne 0 ]; then
  try google/gemini-4-neon "$prompt"; rc=$?
  [ $rc -ne 0 ] && { echo "=== fallback failed rc=$rc ==="; exit 75; }
fi
exit 0
