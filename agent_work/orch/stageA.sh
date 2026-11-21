#!/bin/bash
# Chain, part A, for one chunk under the late-prose ordering (Nick, 2026-11-20):
#   Neon review + Neon adversarial (parallel) -> adversarial run 1 -> one Opus repair ->
#   adversarial run 2 -> Neon re-review (+ attack alignment) -> commit.
# Usage: stageA.sh <chNN> "<title>" "<outline ref>" "<code paths>" <base-rev> [start: review|rereview]
# Ends by writing /root/runs/sqlite-p2-<chNN>.stageA (OK ... or FAILED <where>). Run detached:
#   nohup stageA.sh ... > /root/runs/sqlite-p2-<chNN>-chainA.log 2>&1 &
. "$(dirname "$0")/lib.sh"
ch=$1; title=$2; oref=$3; paths=$4; base=$5; start=${6:-review}
wt=/root/Software/sqlite-internals-work/$ch; cd $wt || exit 1; r=agent_work/$ch/adversarial
out=$RUNS/sqlite-p2-$ch.stageA; rm -f $out
fail() { say "FAILED $*"; echo "FAILED $*" > $out; exit 1; }
export CH=$ch TITLE="$title" OUTLINE_REF="$oref" CODE_PATHS="$paths" BASE=$base WT=$wt CHDIR="" ROUND="" ROUND_NOTE="" REVIEW_FILE=REVIEW-1.md
if [ $start = review ]; then
  $ORCH/mkbrief.sh neon-review $ch-review1 >/dev/null; $ORCH/mkbrief.sh neon-adversarial $ch-adversarial >/dev/null
  printf '\nNote: the code carries no comments yet and there is no chapter text yet (the prose pass runs last). The claims to attack are the outline section, build-notes.md and the evidence files.\n' >> $BR/$ch-adversarial.md
  printf '\nNote: the code carries no comments by design (the prose pass runs last); do not report missing comments.\n' >> $BR/$ch-review1.md
  mkdir -p $r
  ( runstep $ch-review1 $wt neon $BR/$ch-review1.md 60 ) & p1=$!
  ( runstep $ch-adversarial $wt neon $BR/$ch-adversarial.md 90 ) & p2=$!
  wait $p1; a=$?; wait $p2; b=$?
  [ -s agent_work/$ch/REVIEW-1.md ] || fail "review1 produced no REVIEW-1.md (rc=$a)"
  [ -f $r/run-all.sh ] || fail "adversarial produced no run-all.sh (rc=$b)"
  git add agent_work/$ch/REVIEW-1.md && git commit -q --author="$NA" -m "$ch: code review round 1 ($(head -1 agent_work/$ch/REVIEW-1.md))"
  git add $r && git commit -q --author="$NA" -m "$ch: adversarial attacks"
  [ -n "$(git status --porcelain)" ] && { say "stray changes discarded:"; git status --short | head; git checkout -q -- . ; git clean -fdq; }
  gofmt -l . 2>/dev/null | grep -v '^kb/' | head -3; go build ./... 2>&1 | head -5
  advrun $ch 1
  v=$(head -1 agent_work/$ch/REVIEW-1.md); nb=$(grep -c '| BROKEN' $r/results-run1.txt)
  say "review 1: $v; broken=$nb"
  $ORCH/repair.sh $ch "$title" "Under this chunk's ordering Neon has NOT written comments or a chapter yet: the code has no comments and you add none. 'Stale prose for Neon' in your report then lists the facts in build-notes.md that changed (and fix build-notes.md itself, it is your file)." >/dev/null
  rc=$(waitrun $ch-repair 120); say "repair rc=$rc"
  if [ "$rc" != 0 ]; then
    cp $RUNS/sqlite-p2-$ch-repair.log $RUNS/sqlite-p2-$ch-repair.try1.log
    { cat $BR/$ch-repair.md; printf '\n\n## Re-dispatch note\nAn earlier run of this brief ended with code %s. Inspect the worktree (git status, git log) first, keep finished work, complete the rest. Log tail:\n```\n' "$rc"; tail -c 1500 $RUNS/sqlite-p2-$ch-repair.try1.log; printf '\n```\n'; } > $BR/$ch-repair.retry.md
    $ORCH/launch.sh $ch-repair $wt opus $BR/$ch-repair.retry.md >/dev/null; rc=$(waitrun $ch-repair 120); say "repair retry rc=$rc"
    [ "$rc" = 0 ] || fail "repair failed twice"
  fi
  [ -s agent_work/$ch/REPAIR-REPORT.md ] || fail "repair left no REPAIR-REPORT.md"
fi
[ -n "$(git status --porcelain)" ] && { say "uncommitted after repair; committing leftovers as Opus"; git add -A; git commit -qm "$ch: repair round leftovers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"; }
[ -f $r/run-all.sh ] || fail "no run-all.sh for run 2"
advrun $ch 2
{ echo; echo "Run 2 — after the Opus repair round ($(git rev-parse --short HEAD)): $(grep -c '| HELD' $r/results-run2.txt) HELD, $(grep -c '| BROKEN' $r/results-run2.txt) BROKEN (\`results-run2.txt\`)."
  echo "The repair report (\`../REPAIR-REPORT.md\`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:"; echo
  grep '| BROKEN' $r/results-run2.txt | sed 's/^ATTACK //' | awk -F' \\| ' '{printf "- %s — %s\n",$1,substr($2,1,150)}'
} >> $r/RESULTS.md
git add -A $r && git commit -qm "$ch: record adversarial run 2 results

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
PROSE_REV=$(cat $RUNS/sqlite-p2-$ch.prerepair) $ORCH/mkbrief.sh neon-rereview-late $ch-rereview >/dev/null
rm -f agent_work/$ch/REVIEW-2.md
runstep $ch-rereview $wt neon $BR/$ch-rereview.md 75 || say "rereview run did not end cleanly; checking its output"
[ -s agent_work/$ch/REVIEW-2.md ] || fail "rereview produced no REVIEW-2.md"
cp $r/results.txt /tmp/adv-$ch-final.out 2>/dev/null
git add agent_work/$ch/REVIEW-2.md $r && git commit -q --author="$NA" -m "$ch: re-review ($(head -1 agent_work/$ch/REVIEW-2.md)), attack expectations aligned"
[ -n "$(git status --porcelain)" ] && { say "stray changes after rereview discarded:"; git status --short | head; git checkout -q -- . ; git clean -fdq; }
advrun $ch 3
s="OK $(head -1 agent_work/$ch/REVIEW-1.md | cut -c1-20) -> $(head -1 agent_work/$ch/REVIEW-2.md | cut -c1-20); run1 $(grep -c '| HELD' $r/results-run1.txt)/$(grep -c '| BROKEN' $r/results-run1.txt) run2 $(grep -c '| HELD' $r/results-run2.txt)/$(grep -c '| BROKEN' $r/results-run2.txt) run3 $(grep -c '| HELD' $r/results-run3.txt)/$(grep -c '| BROKEN' $r/results-run3.txt) (held/broken); head $(git rev-parse --short HEAD)"
git add -A $r && git commit -qm "$ch: record adversarial run 3 (after attack alignment)

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
say "$s"; echo "$s" > $out
