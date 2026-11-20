#!/usr/bin/env bash
# Show which files get fsync()ed for ONE single-row INSERT commit, per
# journal_mode x synchronous, using the installed sqlite3 CLI under strace -y.
# WAL runs set .dbconfig no_ckpt_on_close so close-time checkpoint is excluded;
# a second WAL line ("wal+close") shows the close-time checkpoint included.
# Usage: fsync-per-commit.sh [scratch-dir]
set -u
SCR=${1:-/tmp/sqlite-research-source-inventory-b/fsync}
mkdir -p "$SCR"
run() { # jm sy extra-dot-cmd label
  local jm=$1 sy=$2 pre=$3 label=$4
  local db="$SCR/t.db"; rm -f "$db" "$db"-*
  sqlite3 "$db" "pragma journal_mode=$jm; create table t(x);" >/dev/null
  local t="$SCR/trace-$label-$sy.txt"
  printf '%s\npragma journal_mode=%s;\npragma synchronous=%s;\ninsert into t values(1);\n' \
    "$pre" "$jm" "$sy" > "$SCR/in.sql"
  strace -qq -y -e trace=fsync,fdatasync,unlink,ftruncate -o "$t" sqlite3 "$db" < "$SCR/in.sql" >/dev/null
  printf "%-9s %-6s : %s\n" "$label" "$sy" "$(sed -E 's/^(f[a-z]*sync|unlink|ftruncate)\(([0-9]+<)?([^>,)]*).*/\1:\3/' "$t" \
      | sed "s|$SCR/||g" | tr '\n' ' ')"
}
for sy in off normal full extra; do
  for jm in delete truncate persist; do run $jm $sy "" $jm; done
  run wal $sy ".dbconfig no_ckpt_on_close on" wal
  run wal $sy "" wal+close
done
