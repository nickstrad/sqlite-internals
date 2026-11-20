#!/usr/bin/env bash
# List the file descriptors a single sqlite3 CLI connection holds (read from /proc/<cli-pid>/fd
# through .shell) for rollback (delete) vs WAL mode: idle after a read, inside a write txn,
# after commit. Usage: fds-per-connection.sh [scratch-dir]
set -u
SCR=${1:-/tmp/sqlite-research-source-inventory-b/fds}; rm -rf "$SCR"; mkdir -p "$SCR"; cd "$SCR"
cat > lsfd.sh <<'H'
cli=$(ps -o ppid= -p $PPID | tr -d ' ')   # lsfd.sh <- sh -c <- sqlite3
[ "$(cat /proc/$cli/comm)" = sqlite3 ] || cli=$PPID
ls -l /proc/$cli/fd | grep -o '/tmp/.*' | sed 's|.*/||' | sort | tr '\n' ' '; echo
H
for jm in delete wal; do
  sqlite3 "$jm.db" "pragma journal_mode=$jm; create table t(x);" >/dev/null
  printf '%s\n' "select count(*) from t;" ".print -- $jm idle-after-read:" ".shell sh lsfd.sh" \
    "begin; insert into t values(1);" ".print -- $jm in-write-txn:" ".shell sh lsfd.sh" "commit;" \
    ".print -- $jm after-commit:" ".shell sh lsfd.sh" | sqlite3 "$jm.db" | grep -v '^0$'
done
