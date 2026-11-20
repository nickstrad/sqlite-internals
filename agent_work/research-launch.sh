#!/bin/bash
# Launch one headless research agent in its own worktree. Usage: sqlite-research-launch.sh <topic> <model>
topic=$1; model=$2
repo=/root/Software/sqlite-internals; wt=/root/Software/sqlite-internals-work/research-$topic
export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:/usr/local/go/bin:$HOME/.deno/bin:$PATH"
[ -d "$wt" ] || git -C "$repo" worktree add -q -b "research/$topic" "$wt" main || exit 97
cd "$wt" || exit 98
echo "$$" > /root/runs/sqlite-research-$topic.pid
IS_SANDBOX=1 claude -p --model "$model" --dangerously-skip-permissions \
  < /root/briefs/sqlite-research-$topic.md > /root/runs/sqlite-research-$topic.log 2>&1
rc=$?
echo $rc > /root/runs/sqlite-research-$topic.exit
echo "$(date -Is) research $topic ($model) exit=$rc" >> /root/runs/sqlite-orchestrator-progress.txt
exit $rc
