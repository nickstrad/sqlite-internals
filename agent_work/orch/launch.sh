#!/bin/bash
# Launch one headless agent run for a Phase 2 step, detached, in a worktree.
# Usage: launch.sh <run-name> <worktree-dir> <seat: opus|neon> <brief-file>
# Writes /root/runs/sqlite-p2-<run-name>.{log,pid,exit}. Indexed in agent_work/INDEX.md.
name=$1; wt=$2; seat=$3; brief=$4
runs=/root/runs; base=$runs/sqlite-p2-$name
export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:/usr/local/go/bin:$HOME/go/bin:$HOME/.deno/bin:$HOME/.opencode/bin:$PATH"
[ -d "$wt" ] || { echo "no worktree $wt" > "$base.log"; echo 98 > "$base.exit"; exit 98; }
[ -f "$brief" ] || { echo "no brief $brief" > "$base.log"; echo 97 > "$base.exit"; exit 97; }
rm -f "$base.exit"
(
  cd "$wt" || exit 98
  echo $BASHPID > "$base.pid"
  case "$seat" in
    opus)
      IS_SANDBOX=1 claude -p --model claude-opus-5-5 --dangerously-skip-permissions < "$brief" > "$base.log" 2>&1
      rc=$? ;;
    neon)
      "$(dirname "$0")/../neon/neon-run.sh" "$wt" "$brief" > "$base.log" 2>&1
      rc=$? ;;
    *) echo "unknown seat $seat" > "$base.log"; rc=96 ;;
  esac
  echo $rc > "$base.exit"
) </dev/null >/dev/null 2>&1 &
disown
echo "launched $name seat=$seat wt=$wt"
