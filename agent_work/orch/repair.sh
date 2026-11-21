#!/bin/bash
# Record adversarial run 1 in RESULTS.md and launch the single Opus repair round.
# Usage: repair.sh <chNN> "<title>" ["<extra instructions>"]
ch=$1; wt=/root/Software/sqlite-internals-work/$ch; here=$(cd "$(dirname "$0")" && pwd)
cd $wt || exit 1; r=agent_work/$ch/adversarial
{ echo "# $2 — adversarial results (orchestrator's record)"; echo
  echo "Run 1 — $(date +%F), on the Opus build, \`run-all.sh\`. Raw output: \`results-run1.txt\`."
  echo "$(grep -c '| HELD' $r/results-run1.txt) HELD, $(grep -c '| BROKEN' $r/results-run1.txt) BROKEN. Every BROKEN line went to the repair round to be fixed or answered:"; echo
  echo "| Attack | What was tried | Expected | Got |"; echo "| --- | --- | --- | --- |"
  grep '| BROKEN' $r/results-run1.txt | sed 's/^ATTACK //; s/ | BROKEN$//; s/EXPECT //; s/GOT //' | awk -F' \\| ' '{printf "| %s | %s | %s | %s |\n",$1,substr($2,1,160),substr($3,1,160),substr($4,1,200)}' | sed 's#/root/Software/sqlite-internals-work/[a-z0-9]*/##g'
} > $r/RESULTS.md
git add -A $r && git commit -qm "$ch: record adversarial run 1 results

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
git rev-parse --short HEAD > /root/runs/sqlite-p2-$ch.prerepair
CH=$ch TITLE="$2" REVIEW_FILE=REVIEW-1.md REPAIR_EXTRA="$3" $here/mkbrief.sh repair $ch-repair >/dev/null
$here/launch.sh $ch-repair $wt opus /root/briefs/sqlite-p2/$ch-repair.md
