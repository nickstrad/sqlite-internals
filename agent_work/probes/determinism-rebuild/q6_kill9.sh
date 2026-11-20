#!/usr/bin/env bash
# Q6: kill -9 a writer mid-stream under synchronous=FULL and OFF (rollback + WAL).
# Writer prints each committed counter; after kill -9 the db must contain every acknowledged
# commit and pass integrity_check. Because a process crash leaves the OS page cache intact,
# synchronous=OFF survives too: kill -9 cannot tell the sync settings apart.
set -uo pipefail
D=${1:-/tmp/sqlite-research-determinism-rebuild/q6}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
for jm in DELETE WAL; do for sync in FULL OFF; do
  for trial in 1 2 3; do
    f=k_${jm}_${sync}_$trial.db
    sqlite3 $f "PRAGMA journal_mode=$jm; CREATE TABLE c(n INTEGER PRIMARY KEY, pad BLOB);" >/dev/null
    python3 -I - $f $jm $sync > ack_$f.txt <<'PY' &
import sqlite3, sys
db = sqlite3.connect(sys.argv[1], isolation_level=None)
db.execute(f"PRAGMA journal_mode={sys.argv[2]}"); db.execute(f"PRAGMA synchronous={sys.argv[3]}")
n = 0
while True:
    n += 1
    db.execute("BEGIN"); db.execute("INSERT INTO c VALUES(?, randomblob(3000))", (n,)); db.execute("UPDATE c SET pad=randomblob(3000) WHERE n=?", (max(1,n//2),)); db.execute("COMMIT")
    print(n, flush=True)
PY
    P=$!; sleep 0.$((3+trial)); kill -9 $P; wait $P 2>/dev/null
    ack=$(tail -1 ack_$f.txt); have=$(sqlite3 $f "select max(n) from c"); ic=$(sqlite3 $f "pragma integrity_check")
    printf '%-6s sync=%-4s trial %d: last acked=%-5s max(n) in db=%-5s integrity=%s %s\n' $jm $sync $trial "$ack" "$have" "$ic" "$([ "$have" -ge "$ack" ] && echo OK || echo LOST-ACKED-COMMIT)"
  done
done; done
python3 -I -c "import sqlite3;print('python sqlite3 module links SQLite', sqlite3.sqlite_version, '(writer library for this probe only)')"
