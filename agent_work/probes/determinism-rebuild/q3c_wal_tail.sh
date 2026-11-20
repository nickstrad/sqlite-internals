#!/usr/bin/env bash
# Q3c edge cases for "replay WAL frames yourself":
#  (1) uncommitted frames at the WAL tail (cache spill of an open transaction) must be ignored;
#  (2) a corrupted frame in the last transaction: our replay and SQLite's own WAL recovery
#      (open db + damaged -wal, checkpoint) must agree;
#  (3) page-1 header fields after replay vs base.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
D=${1:-/tmp/sqlite-research-determinism-rebuild/q3c}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
WR=${WALREPLAY:-/tmp/sqlite-research-determinism-rebuild/walreplay}
{ echo "PRAGMA page_size=4096; PRAGMA journal_mode=WAL;"; cat "$HERE/seed.sql"
  echo "PRAGMA wal_checkpoint(TRUNCATE);"; echo ".shell cp t.db t.base"
  echo "PRAGMA wal_autocheckpoint=0;"; cat "$HERE/phase2.sql"
  echo "PRAGMA cache_size=5;"
  echo "BEGIN; WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<3000) INSERT INTO orders SELECT 100000+i, 1, printf('%.*c', 300, 'z'), i FROM n;"
  echo ".shell cp t.db-wal t.wal"
  echo "ROLLBACK;"
  echo ".shell cp t.db-wal t.wal-after-rollback"
  echo "PRAGMA wal_checkpoint(TRUNCATE);"; echo ".shell cp t.db t.ckpt"; } | sqlite3 t.db >/dev/null
echo "(1) WAL copied mid-transaction (spilled, uncommitted frames at tail):"
"$WR" frames t.wal | tail -1
"$WR" apply t.base t.wal t.replayed
echo "    replay vs SQLite checkpoint: $(cmp -s t.replayed t.ckpt && echo BYTE-IDENTICAL || echo DIFFER)"
"$WR" frames t.wal-after-rollback | tail -1
echo "(2) corrupt one byte inside the page image of the last commit frame:"
last=$("$WR" frames t.wal | grep -n 'commit=[1-9]' | tail -1 | cut -d: -f1)   # line number = frame index+1 (header line)
fi=$((last-1)); off=$((32 + (fi-1)*(24+4096) + 24 + 100))
cp t.wal bad.wal; printf '\xff' | dd of=bad.wal bs=1 seek=$off conv=notrunc status=none
"$WR" apply t.base bad.wal bad.replayed
cp t.base s.db; cp bad.wal s.db-wal
sqlite3 s.db "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null
echo "    our replay vs SQLite recovery+checkpoint of the same damaged WAL: $(cmp -s bad.replayed s.db && echo BYTE-IDENTICAL || echo DIFFER)"
echo "    damaged-wal result vs full result: $(cmp -s bad.replayed t.ckpt && echo SAME || echo 'DIFFERENT (last txn lost, as expected)')"
echo "    rows in orders: full=$(sqlite3 t.ckpt 'select count(*) from orders') damaged=$(sqlite3 bad.replayed 'select count(*) from orders') ; integrity: $(sqlite3 bad.replayed 'pragma integrity_check')"
echo "(3) header fields base vs replayed:"; python3 -I "$HERE/hdr.py" t.base t.replayed | cut -c1-200
