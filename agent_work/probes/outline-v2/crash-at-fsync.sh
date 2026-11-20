#!/usr/bin/env bash
# Probe (outline revision, Phase 1b): deterministic crash points with no custom
# C shim. strace's --inject delivers SIGKILL on entry to the Nth fsync/fdatasync
# of the writer thread. The victim is the unmodified modernc driver.
# Usage: crash-at-fsync.sh   (builds the victim, prints one line per case)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d /tmp/outline-v2-crash.XXXXXX)
trap 'rm -rf "$work"' EXIT
(cd "$here" && go build -o "$work/victim" ./victim)

# Baseline: fsync/fdatasync calls for 5 commits at each level, no injection.
for sync in NORMAL FULL; do
  rm -f "$work"/b.db*
  n=$(strace -f -e trace=fsync,fdatasync -o "$work/trace" "$work/victim" write "$work/b.db" $sync 5 >/dev/null; grep -c -E 'f(data)?sync\(' "$work/trace" || true)
  echo "baseline sync=$sync commits=5 fsync_calls=$n"
done

# Kill on entry to the Kth sync call, 5 runs per K; report acks seen and rows after reopen.
for sync in FULL NORMAL; do
  for k in 4 6 7 8 9 10; do
    out=""
    for run in 1 2 3 4 5; do
      rm -f "$work"/c.db*
      set +e
      ( strace -f -e trace=fsync,fdatasync -e inject=fsync,fdatasync:signal=SIGKILL:when=$k \
        -o "$work/trace" "$work/victim" write "$work/c.db" $sync 5 > "$work/out" 2>/dev/null ) 2>/dev/null
      rc=$?
      set -e
      acks=$(grep -c '^ack' "$work/out" || true)
      rows=$(sqlite3 "$work/c.db" "select count(*) from t" 2>/dev/null || echo "no-table")
      ok=$(sqlite3 "$work/c.db" "pragma integrity_check" 2>/dev/null || echo "?")
      out="$out [rc=$rc acks=$acks rows=$rows integrity=$ok]"
    done
    echo "sync=$sync kill_at_sync=$k:$out"
  done
done
