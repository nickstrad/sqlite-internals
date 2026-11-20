#!/usr/bin/env bash
# Q3 under an active writer: (1) copying a WAL db without its -wal loses commits;
# (2) a slow unlocked file copy of a rollback-journal db under a writer gives torn copies;
# (3) sqlite_dbpage inside a read txn is a consistent snapshot; (4) .backup under writes.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
D=${1:-/tmp/sqlite-research-determinism-rebuild/q3conc}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
echo "(1) lost commits: WAL db, writer still open, copy only the .db file"
{ echo "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"; cat "$HERE/seed.sql"
  echo ".shell cp w.db w_only.db; cp w.db-wal w_only_wal_saved"; echo "SELECT 'live rows:', count(*) FROM orders;"; } | sqlite3 w.db
echo "   copy without -wal: orders rows = $(sqlite3 w_only.db 'select count(*) from orders' 2>&1)  tables=$(sqlite3 w_only.db 'select count(*) from sqlite_schema' 2>&1)"
cp w_only_wal_saved w_only.db-wal; echo "   same copy + its -wal: orders rows = $(sqlite3 w_only.db 'select count(*) from orders')"
echo "(2) torn copy: rollback-journal db (~13 MB), writer rewrites all rows repeatedly; slow block copy"
sqlite3 r.db "PRAGMA journal_mode=DELETE; CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB, gen INT); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<12000) INSERT INTO t SELECT i, randomblob(1000), 0 FROM n; CREATE INDEX tg ON t(gen);" >/dev/null
( for g in $(seq 1 400); do sqlite3 r.db "PRAGMA synchronous=OFF; UPDATE t SET gen=$g, v=randomblob(1000);" 2>/dev/null; done ) & WP=$!
bad=0; tot=0
for i in $(seq 1 8); do python3 -I "$HERE/torn_copy.py" r.db torn$i.db 0.0002; tot=$((tot+1))
  res=$(sqlite3 torn$i.db "PRAGMA integrity_check;" 2>&1 | head -2 | tr '\n' ' '); gens=$(sqlite3 torn$i.db "select count(distinct gen) from t" 2>&1)
  [ "$res" = "ok " ] || bad=$((bad+1)); echo "   copy $i: integrity=[${res:0:70}] distinct gen values=$gens"; done
kill $WP 2>/dev/null; wait $WP 2>/dev/null || true
echo "   torn copies: $bad of $tot failed integrity_check"
echo "(3) sqlite_dbpage capture in one read txn while another process commits (WAL)"
{ echo "PRAGMA journal_mode=WAL;"; cat "$HERE/seed.sql"; } | sqlite3 s.db >/dev/null
sqlite3 s.db "VACUUM INTO 'before.db'" ; mkdir -p pg
sqlite3 s.db "BEGIN; SELECT count(*) FROM orders;" ".shell sqlite3 s.db 'DELETE FROM orders WHERE id<3000; INSERT INTO customer(name) VALUES(1)'" \
  "SELECT count(writefile(printf('pg/%08d',pgno), data)) FROM sqlite_dbpage;" "COMMIT;" >/dev/null
cat pg/* > snap.db
echo "   capture: integrity=$(sqlite3 snap.db 'pragma integrity_check') orders=$(sqlite3 snap.db 'select count(*) from orders') ; live now: orders=$(sqlite3 s.db 'select count(*) from orders')"
echo "   logical hash capture == pre-write state: $([ "$(sqlite3 snap.db .sha3sum)" = "$(sqlite3 before.db .sha3sum)" ] && echo yes || echo no)"
echo "(4) .backup (step 100 pages) of a WAL db while another process commits every ~20 ms"
sqlite3 big.db "PRAGMA journal_mode=WAL; CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<20000) INSERT INTO t SELECT i, randomblob(1000) FROM n;" >/dev/null
( for g in $(seq 1 3000); do sqlite3 big.db "INSERT INTO t(v) VALUES(randomblob(100));" 2>/dev/null; sleep 0.02; done ) & WP=$!
for i in 1 2 3; do s=$(date +%s.%N); timeout 60 sqlite3 big.db ".backup bk$i.db"; rc=$?; e=$(date +%s.%N)
  echo "   backup $i: rc=$rc secs=$(printf '%.2f' $(echo "$e-$s"|bc)) integrity=$(sqlite3 bk$i.db 'pragma integrity_check' 2>&1|head -1) rows=$(sqlite3 bk$i.db 'select count(*) from t' 2>&1)"; done
kill $WP 2>/dev/null; wait $WP 2>/dev/null || true
echo "(4b) same, writer commits back-to-back (no sleep); 30 s limit"
( while true; do sqlite3 big.db "INSERT INTO t(v) VALUES(randomblob(100));" 2>/dev/null; done ) & WP=$!
s=$(date +%s.%N); timeout 30 sqlite3 big.db ".backup bk4.db"; rc=$?; e=$(date +%s.%N)
echo "   backup 4b: rc=$rc secs=$(printf '%.2f' $(echo "$e-$s"|bc)) integrity=$(sqlite3 bk4.db 'pragma integrity_check' 2>&1|head -1)"
kill $WP 2>/dev/null; wait $WP 2>/dev/null || true
exit 0
