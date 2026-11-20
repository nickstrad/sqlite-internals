#!/bin/bash
# Run Gemini 4 Neon as an agent (OpenCode) inside a directory with a brief file as the prompt.
# Usage: neon-run.sh <dir> <brief-file>
# PRIMARY route: OpenRouter (openrouter/google/gemini-4-neon). FALLBACK: direct Gemini API
# (google/gemini-4-neon) when the primary exits non-zero or prints a provider error.
# A 429 on the fallback is reported (exit 75), not treated as a pipeline bug.
dir=$1; brief=$2
set -a; source /root/Software/.secrets/yt-transcribe.env; source /root/Software/.secrets/gemini-neon.env; set +a
cd "$dir" || exit 98
prompt="Read the brief file at $brief and carry it out completely in this directory ($dir). The brief is your full instruction."
try() {
  echo "=== neon-run route=$1 $(date -Is) ==="
  timeout 5400 opencode run --auto -m "$1" "$prompt" 2>&1 | tee /tmp/neon-run.$$.out
  rc=${PIPESTATUS[0]}
  if [ $rc -eq 0 ] && grep -qiE "insufficient credits|rate limit exceeded|quota exceeded|\b(401|402|429)\b.*(error|openrouter|quota)|ProviderError|AI_APICallError" /tmp/neon-run.$$.out && ! grep -q "NEON-DONE" /tmp/neon-run.$$.out; then rc=70; fi
  rm -f /tmp/neon-run.$$.out
  return $rc
}
try openrouter/google/gemini-4-neon; rc=$?
if [ $rc -ne 0 ]; then
  echo "=== primary failed rc=$rc; trying fallback ==="
  try google/gemini-4-neon; rc=$?
  [ $rc -ne 0 ] && { echo "=== fallback failed rc=$rc ==="; exit 75; }
fi
exit 0
