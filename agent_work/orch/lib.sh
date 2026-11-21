#!/bin/bash
# Shared helpers for the chain scripts (stageA.sh, stageB.sh). Sourced, not run.
export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:/usr/local/go/bin:$HOME/go/bin:$HOME/.deno/bin:$HOME/.opencode/bin:$PATH"
export SQLITE_SRC=/root/Software/sqlite-src/3.53.4
ORCH=/root/Software/sqlite-internals/agent_work/orch; BR=/root/briefs/sqlite-p2; RUNS=/root/runs
NA='Gemini 4 Neon (via OpenCode) <noreply@google.com>'
say() { echo "$(date -Is) $*"; }
# waitrun <run-name> <minutes>: block until the run's exit file exists; echo its code.
# 99 = died without an exit file, 98 = deadline passed (the run is killed).
waitrun() { local b=$RUNS/sqlite-p2-$1 end=$((SECONDS+$2*60))
  sleep 15
  while [ $SECONDS -lt $end ]; do
    [ -f $b.exit ] && { cat $b.exit; return; }
    kill -0 "$(cat $b.pid 2>/dev/null)" 2>/dev/null || { sleep 5; [ -f $b.exit ] && { cat $b.exit; return; }; echo 99; return; }
    sleep 20
  done
  pkill -P "$(cat $b.pid)" 2>/dev/null; kill "$(cat $b.pid)" 2>/dev/null; echo 98; }
# runstep <run-name> <worktree> <seat> <brief> <minutes>: launch, wait, and on failure
# re-dispatch ONCE (Neon is told the earlier run was cut off; Opus gets the log tail appended).
runstep() { local n=$1 wt=$2 seat=$3 brief=$4 mins=$5 rc
  $ORCH/launch.sh $n $wt $seat $brief >/dev/null; rc=$(waitrun $n $mins); say "$n first try rc=$rc"
  [ "$rc" = 0 ] && return 0
  cp $RUNS/sqlite-p2-$n.log $RUNS/sqlite-p2-$n.try1.log 2>/dev/null
  if [ $seat = opus ]; then
    { cat $brief; printf '\n\n## Re-dispatch note\nAn earlier run of this brief ended with code %s. Inspect the worktree (git status, git log) first, keep finished work, and complete the rest. Log tail of the failed run:\n```\n' "$rc"; tail -c 1500 $RUNS/sqlite-p2-$n.try1.log; printf '\n```\n'; } > $brief.retry.md
    $ORCH/launch.sh $n $wt $seat $brief.retry.md >/dev/null
  else NEON_RESUME=1 $ORCH/launch.sh $n $wt $seat $brief >/dev/null; fi
  rc=$(waitrun $n $mins); say "$n second try rc=$rc"; [ "$rc" = 0 ]; }
advrun() { # advrun <ch> <n>: run the adversarial suite, keep raw output as results-run<n>.txt
  local r=agent_work/$1/adversarial
  timeout 2400 bash $r/run-all.sh > /tmp/adv-$1-$2.out 2>&1; local rc=$?
  cp /tmp/adv-$1-$2.out $r/results-run$2.txt; git checkout -q -- $r/results.txt 2>/dev/null
  say "adversarial run $2 rc=$rc held=$(grep -c '| HELD' $r/results-run$2.txt) broken=$(grep -c '| BROKEN' $r/results-run$2.txt)"; }
