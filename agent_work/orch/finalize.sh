#!/bin/bash
# Commit Neon's re-review/touch-up under Neon's name and run the acceptance gate.
# Usage: finalize.sh <chNN> [chapter-dir]   (output: /root/runs/sqlite-p2-<chNN>-accept.txt)
ch=$1; chdir=$2; cd /root/Software/sqlite-internals-work/$ch || exit 1
NA='Gemini 4 Neon (via OpenCode) <noreply@google.com>'
git add -A && git commit -q --author="$NA" -m "$ch: re-review ($(head -1 agent_work/$ch/REVIEW-2.md)), prose touch-up after repair, attack expectations aligned"
/root/Software/sqlite-internals/agent_work/orch/accept.sh $ch $(cat /root/runs/sqlite-p2-$ch.prefinal) $chdir 2>&1 | grep -aE "^(PASS|FAIL|INFO|ACCEPT|    )" | cut -c1-240 > /root/runs/sqlite-p2-$ch-accept.txt
cat /root/runs/sqlite-p2-$ch-accept.txt
