. /root/Software/sqlite-internals/agent_work/orch/lib.sh
ch=pub; wt=/root/Software/sqlite-internals-work/pub; cd $wt; r=agent_work/pub/adversarial; out=$RUNS/sqlite-p2-pub.stageA2; rm -f $out
runstep pub-repair2 $wt opus $BR/pub-repair2.md 120 || { echo "FAILED repair2" > $out; exit 1; }
[ -n "$(git status --porcelain)" ] && { git add -A; git commit -qm "pub: repair round 2 leftovers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"; }
advrun pub 4
git add -A $r && git commit -qm "pub: record adversarial run 4 (after repair round 2)

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
rm -f agent_work/pub/REVIEW-3.md
runstep pub-rereview2 $wt neon $BR/pub-rereview2.md 75 || say "rereview2 did not end cleanly"
[ -s agent_work/pub/REVIEW-3.md ] || { echo "FAILED no REVIEW-3.md" > $out; exit 1; }
git add agent_work/pub/REVIEW-3.md $r && git commit -q --author="$NA" -m "pub: re-review 2 ($(head -1 agent_work/pub/REVIEW-3.md)), attack expectations aligned"
[ -n "$(git status --porcelain)" ] && { git status --short | head; git checkout -q -- . ; git clean -fdq; }
advrun pub 5
git add -A $r && git commit -qm "pub: record adversarial run 5

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
echo "OK $(head -1 agent_work/pub/REVIEW-3.md); run4 $(grep -c '| HELD' $r/results-run4.txt)/$(grep -c '| BROKEN' $r/results-run4.txt) run5 $(grep -c '| HELD' $r/results-run5.txt)/$(grep -c '| BROKEN' $r/results-run5.txt); head $(git rev-parse --short HEAD)" > $out
