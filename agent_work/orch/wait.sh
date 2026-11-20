#!/bin/bash
# Block until every named run has an exit file, or ~9.5 minutes pass. Prints status lines.
# Usage: wait.sh <run-name>...
end=$((SECONDS+570))
while [ $SECONDS -lt $end ]; do
  all=1; for n in "$@"; do [ -f /root/runs/sqlite-p2-$n.exit ] || all=0; done
  [ $all = 1 ] && break; sleep 15
done
for n in "$@"; do
  b=/root/runs/sqlite-p2-$n
  if [ -f $b.exit ]; then echo "$n: exit=$(cat $b.exit)"
  elif kill -0 "$(cat $b.pid 2>/dev/null)" 2>/dev/null; then echo "$n: running (log $(wc -c < $b.log) bytes)"
  else echo "$n: DEAD without exit file"; fi
done
