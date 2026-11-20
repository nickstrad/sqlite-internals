#!/usr/bin/env bash
# Q1c: SQL-level nondeterminism sources. Each case is run twice into fresh dbs; reports whether
# the resulting files are byte-identical.
set -uo pipefail
D=${1:-/tmp/sqlite-research-determinism-rebuild/q1c}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
case_() { local name=$1 sql=$2; for r in 1 2; do rm -f $name.$r.db; sqlite3 $name.$r.db "$sql" >/dev/null 2>&1; sleep 1.1; done
  local a b; a=$(sha256sum < $name.1.db|cut -c1-12); b=$(sha256sum < $name.2.db|cut -c1-12)
  printf '%-26s %s\n' $name "$([ $a = $b ] && echo IDENTICAL || echo DIFFER)"; }
T="CREATE TABLE t(id INTEGER PRIMARY KEY, v);"
case_ random        "$T INSERT INTO t(v) VALUES(random());"
case_ randomblob    "$T INSERT INTO t(v) VALUES(randomblob(16));"
case_ now           "$T INSERT INTO t(v) VALUES(current_timestamp);"
case_ max_rowid     "$T INSERT INTO t VALUES(9223372036854775807,'max'); INSERT INTO t(v) VALUES('next'),('next2');"
case_ autoincrement "CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, v); INSERT INTO t(v) VALUES(1),(2); DELETE FROM t; INSERT INTO t(v) VALUES(3);"
case_ analyze       "$T CREATE INDEX tv ON t(v); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<5000) INSERT INTO t(v) SELECT i%37 FROM n; ANALYZE;"
case_ float_text    "$T INSERT INTO t(v) VALUES(CAST(0.1+0.2 AS TEXT)), (printf('%.17g',0.1+0.2)), (1e300*10);"
echo "--- max rowid: rows after 9223372036854775807 get a random rowid (run twice):"
for r in 1 2; do sqlite3 max_rowid.$r.db "SELECT id,v FROM t ORDER BY v" | tr '\n' ' '; echo; done
echo "--- unordered SELECT feeding INSERT: same logical rows, rowids depend on scan order chosen by planner"
sqlite3 :memory: "CREATE TABLE src(k TEXT PRIMARY KEY, v) WITHOUT ROWID; CREATE TABLE src2(id INTEGER PRIMARY KEY, k TEXT, v); INSERT INTO src2(k,v) VALUES('b',1),('a',2),('c',3); INSERT INTO src SELECT k,v FROM src2;
CREATE TABLE dst(id INTEGER PRIMARY KEY, k); INSERT INTO dst(k) SELECT k FROM src2; SELECT 'no index:', group_concat(id||'='||k,' ') FROM dst;
DELETE FROM dst; CREATE INDEX src2k ON src2(k); INSERT INTO dst(k) SELECT k FROM src2 WHERE k>''; SELECT 'with index on k:', group_concat(id||'='||k,' ') FROM dst;"
echo "--- autoincrement leaves sqlite_sequence state:"; sqlite3 autoincrement.1.db "SELECT * FROM sqlite_sequence; SELECT * FROM t;"
echo "--- float text:"; sqlite3 float_text.1.db "SELECT v, typeof(v) FROM t;"
echo "--- persistent vs per-connection pragmas (set, close, reopen, read back):"
rm -f p.db; sqlite3 p.db "PRAGMA page_size=8192; PRAGMA auto_vacuum=INCREMENTAL; PRAGMA user_version=7; PRAGMA application_id=42; PRAGMA journal_mode=WAL; PRAGMA encoding='UTF-16le'; PRAGMA synchronous=OFF; PRAGMA cache_size=5; PRAGMA foreign_keys=ON; PRAGMA secure_delete=ON; PRAGMA default_cache_size=77; CREATE TABLE x(a);" >/dev/null 2>&1
for p in page_size auto_vacuum user_version application_id journal_mode encoding synchronous cache_size foreign_keys secure_delete default_cache_size; do printf '  %-18s %s\n' $p "$(sqlite3 p.db "PRAGMA $p;" 2>&1 | tr '\n' ' ')"; done
