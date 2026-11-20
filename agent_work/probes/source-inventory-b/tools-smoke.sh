#!/usr/bin/env bash
# Smoke-test the ch.9 tools built by build-tools.sh: showwal on a live WAL,
# dbhash stable across VACUUM INTO, sqldiff empty between copies, .recover runs,
# sqlite3_rsync local copy. Usage: tools-smoke.sh [bin-dir] [scratch-dir]
set -u
B=${1:-/tmp/sqlite-research-source-inventory-b/src-copy}
SCR=${2:-/tmp/sqlite-research-source-inventory-b/smoke}; rm -rf "$SCR"; mkdir -p "$SCR"; cd "$SCR"
"$B/sqlite3" a.db ".dbconfig no_ckpt_on_close on" "pragma journal_mode=wal" \
  "create table t(x)" "insert into t values(1)" "insert into t values(2)" >/dev/null
echo "== showwal a.db-wal (header + frame lines)"; "$B/showwal" a.db-wal | head -12
echo "== VACUUM INTO + dbhash"
"$B/sqlite3" a.db "vacuum into 'b.db'"
echo "a: $("$B/dbhash" a.db)"; echo "b: $("$B/dbhash" b.db)"
echo "== sqldiff a b (expect empty)"; "$B/sqldiff" a.db b.db; echo "rc=$?"
echo "== sqlite3_rsync local a -> c"; "$B/sqlite3_rsync" a.db c.db; echo "rc=$? c: $("$B/dbhash" c.db)"
echo "== .recover"; "$B/sqlite3" b.db .recover | grep -c INSERT
echo "== integrity_check"; "$B/sqlite3" c.db "pragma integrity_check"
