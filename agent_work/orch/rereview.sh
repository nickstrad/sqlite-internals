#!/bin/bash
# After the Opus repair: rerun the adversarial suite (results-run2.txt), record it, and launch
# Neon's re-review + prose touch-up + attack alignment + "What attacked this chapter" text.
# Usage: rereview.sh <chNN> "<title>" "<code paths>" [chapter-dir]
ch=$1; title=$2; paths=$3; chdir=$4; wt=/root/Software/sqlite-internals-work/$ch; here=$(cd "$(dirname "$0")" && pwd)
cd $wt || exit 1; r=agent_work/$ch/adversarial; export SQLITE_SRC=/root/Software/sqlite-src/3.53.4
test -z "$(git status --porcelain)" || { echo "worktree not clean:"; git status --short | head; }
bash $r/run-all.sh > /tmp/adv-$ch-2.out 2>&1; echo "adv run2 rc=$? held=$(grep -c '| HELD' /tmp/adv-$ch-2.out) broken=$(grep -c '| BROKEN' /tmp/adv-$ch-2.out)"
grep '| BROKEN' /tmp/adv-$ch-2.out | cut -c1-140
cp /tmp/adv-$ch-2.out $r/results-run2.txt; git checkout -q -- $r/results.txt 2>/dev/null
{ echo; echo "Run 2 — after the Opus repair round ($(git rev-parse --short HEAD)): $(grep -c '| HELD' $r/results-run2.txt) HELD, $(grep -c '| BROKEN' $r/results-run2.txt) BROKEN (\`results-run2.txt\`)."
  echo "The repair report (\`../REPAIR-REPORT.md\`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:"; echo
  grep '| BROKEN' $r/results-run2.txt | sed 's/^ATTACK //' | awk -F' \\| ' '{printf "- %s — %s\n",$1,substr($2,1,150)}'
} >> $r/RESULTS.md
git add -A $r && git commit -qm "$ch: record adversarial run 2 results

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
git rev-parse --short HEAD > /root/runs/sqlite-p2-$ch.prefinal
touch=""
if [ -n "$chdir" ]; then touch="
## Part 3 — the chapter and the attacks
Chapter: $chdir/$chdir.md (and answers.md).
1. Correct every sentence, number, table and rule the repair made stale (REPAIR-REPORT.md
   'Stale prose for Neon' and any build-notes addendum). Where the repair narrowed or corrected
   a claim, the chapter must state the corrected claim. Evidence files are authoritative.
   If the repair added code files or packages, inline/describe them as your prose pass would.
2. Adversarial attacks (agent_work/$ch/adversarial/): results after repair are in
   results-run2.txt, the orchestrator's record in RESULTS.md. For each attack still BROKEN:
   if the repair report shows the attack's EXPECT line is stale or states a claim the chapter
   no longer makes, update that attack so it asserts the corrected claim; do NOT weaken any
   attack that tests a claim the chapter still makes — if such an attack is still BROKEN, leave
   it BROKEN and report it as a new finding in REVIEW-2.md (that makes the verdict FAIL).
   Re-run 'bash agent_work/$ch/adversarial/run-all.sh' and leave results.txt.
3. In section 6, under '### What attacked this chapter', keep the '<!-- adversarial-results -->'
   marker line first and write a short factual account after it: which script attacked which
   claim (relative links ../agent_work/$ch/adversarial/<name>/main.go), what run 1 found,
   what was fixed, and which claims the chapter narrowed because of it.
4. Run 'go run ./cmd/course inline $chdir/$chdir.md' then the '-check' form.
"; fi
CH=$ch TITLE="$title" WT=$wt PROSE_REV=$(cat /root/runs/sqlite-p2-$ch.prerepair) CODE_PATHS="$paths" CHAPTER_TOUCHUP="$touch" $here/mkbrief.sh neon-rereview $ch-rereview >/dev/null
$here/launch.sh $ch-rereview $wt neon /root/briefs/sqlite-p2/$ch-rereview.md
