#!/usr/bin/env bash
# WAL mode: fsync()s and WAL pwrite offsets for two consecutive autocommit INSERTs
# (first one restarts a fresh -wal), synchronous=NORMAL vs FULL. Installed sqlite3 CLI.
# Usage: wal-steady-sync.sh [scratch-dir]
set -u
SCR=${1:-/tmp/sqlite-research-source-inventory-b/walsync}; mkdir -p "$SCR"; cd "$SCR"
for sy in normal full; do
  rm -f s.db s.db-wal s.db-shm
  sqlite3 s.db "pragma journal_mode=wal; create table t(x); insert into t values(0);" >/dev/null
  printf '.dbconfig no_ckpt_on_close on\npragma synchronous=%s;\ninsert into t values(1);\ninsert into t values(2);\n' $sy > in.sql
  strace -qq -y -e trace=fsync,fdatasync,pwrite64 -o tr sqlite3 s.db < in.sql >/dev/null
  echo "== synchronous=$sy (shm extension writes omitted)"
  grep -v 'db-shm' tr | sed -E 's/^(pwrite64)\([0-9]+<[^>]*\/([^/>]+)>, "(.{0,16}).*", ([0-9]+), ([0-9]+)\).*/\1 \2 len=\4 off=\5 head=\3/; s/^(f[a-z]*sync)\([0-9]+<([^>]*)>\).*/\1 \2/'
done
