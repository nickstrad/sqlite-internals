#!/usr/bin/env bash
# Q4: equality ladder on the q3 artifacts. Needs q3_capture_methods.sh run first and
# dbhash/sqldiff built from the pinned tree in $BLD (see README).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); BLD=${BLD:-/tmp/sqlite-research-determinism-rebuild/bld}
Q3=${1:-/tmp/sqlite-research-determinism-rebuild/q3}; cd "$Q3"
echo "file               sha256(16)        pagediff-vs-src(ignhdr)  dbhash(16)        sha3sum(16)       integrity  fk_check"
for f in src.db a_copy.db b_dbpage.db d_backup.db e_vac1.db g_replay1.db; do
  printf '%-18s %s  %-23s  %s  %s  %-9s  %s\n' $f "$(sha256sum < $f | cut -c1-16)" \
    "$(python3 -I $HERE/pagediff.py src.db $f --ignore-header | tail -1 | sed 's/differing pages: /pages:/')" \
    "$($BLD/dbhash $f | cut -c1-16)" "$(sqlite3 $f .sha3sum | cut -c1-16)" \
    "$(sqlite3 $f 'pragma integrity_check')" "rows=$(sqlite3 $f 'pragma foreign_key_check' | wc -l)"
done
echo "--- sqldiff src.db g_replay1.db (expect empty):"; $BLD/sqldiff src.db g_replay1.db | head -3; echo "(end)"
echo "--- sqldiff --summary between seed-only base and src (seed+phase2):"
{ echo "PRAGMA page_size=4096;"; cat "$HERE/seed.sql"; } | sqlite3 base_only.db >/dev/null; $BLD/sqldiff --summary base_only.db src.db
echo "--- blind spots of content hashes"
mk() { rm -f $1; sqlite3 $1 "$2"; }
mk r1.db "CREATE TABLE t(a); INSERT INTO t(rowid,a) VALUES(1,'x'),(2,'y');"
mk r2.db "CREATE TABLE t(a); INSERT INTO t(rowid,a) VALUES(5,'x'),(9,'y');"
echo "implicit rowid differs (1,2 vs 5,9): dbhash equal=$([ "$($BLD/dbhash r1.db|cut -c1-40)" = "$($BLD/dbhash r2.db|cut -c1-40)" ] && echo YES || echo no)  sha3sum equal=$([ "$(sqlite3 r1.db .sha3sum)" = "$(sqlite3 r2.db .sha3sum)" ] && echo YES || echo no)  sqldiff: $($BLD/sqldiff r1.db r2.db | head -2 | tr '\n' ' ')"
mk s1.db "CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, a); INSERT INTO t(a) VALUES('x');"
mk s2.db "CREATE TABLE t(id INTEGER PRIMARY KEY AUTOINCREMENT, a); INSERT INTO t(a) VALUES('x'),('tmp'); DELETE FROM t WHERE a='tmp';"
echo "sqlite_sequence differs (1 vs 2): dbhash equal=$([ "$($BLD/dbhash s1.db|cut -c1-40)" = "$($BLD/dbhash s2.db|cut -c1-40)" ] && echo YES || echo no)  sha3sum equal=$([ "$(sqlite3 s1.db .sha3sum)" = "$(sqlite3 s2.db .sha3sum)" ] && echo YES || echo no)  sqldiff: [$($BLD/sqldiff s1.db s2.db | head -2 | tr '\n' ' ')]"
mk u1.db "PRAGMA user_version=1; CREATE TABLE t(a);"; mk u2.db "PRAGMA user_version=2; CREATE TABLE t(a);"
echo "user_version differs: dbhash equal=$([ "$($BLD/dbhash u1.db|cut -c1-40)" = "$($BLD/dbhash u2.db|cut -c1-40)" ] && echo YES || echo no)  sha3sum equal=$([ "$(sqlite3 u1.db .sha3sum)" = "$(sqlite3 u2.db .sha3sum)" ] && echo YES || echo no)"
mk f1.db "CREATE TABLE t(a); INSERT INTO t VALUES(1);"; mk f2.db "CREATE TABLE t(a); INSERT INTO t VALUES(1.0);"
echo "1 (INTEGER) vs 1.0 (REAL): dbhash equal=$([ "$($BLD/dbhash f1.db|cut -c1-40)" = "$($BLD/dbhash f2.db|cut -c1-40)" ] && echo YES || echo no)  sha3sum equal=$([ "$(sqlite3 f1.db .sha3sum)" = "$(sqlite3 f2.db .sha3sum)" ] && echo YES || echo no)  sqldiff: [$($BLD/sqldiff f1.db f2.db | head -2 | tr '\n' ' ')]"
