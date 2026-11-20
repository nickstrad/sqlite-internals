#!/usr/bin/env bash
# Q3f: sqlite3_serialize vs the file. Needs $SER (go build -o $SER ./go/serialize) and q3 run first.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); SER=${SER:-/tmp/sqlite-research-determinism-rebuild/serialize}
D=${1:-/tmp/sqlite-research-determinism-rebuild/q3f}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
{ echo "PRAGMA page_size=4096;"; cat "$HERE/seed.sql"; cat "$HERE/phase2.sql"; } | sqlite3 rb.db >/dev/null
"$SER" rb.db rb.ser; echo "rollback db: serialize vs file: $(cmp -s rb.db rb.ser && echo BYTE-IDENTICAL || echo DIFFER)"
{ echo "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"; cat "$HERE/seed.sql"; cat "$HERE/phase2.sql"
  echo ".shell $SER w.db w.ser >/dev/null"; echo ".shell cp w.db w.pre"; echo "PRAGMA wal_checkpoint(TRUNCATE);"; } | sqlite3 w.db >/dev/null
echo "WAL db, frames not checkpointed: serialize vs db file at that moment: $(cmp -s w.pre w.ser && echo BYTE-IDENTICAL || echo DIFFER); vs db after TRUNCATE checkpoint: $(cmp -s w.db w.ser && echo BYTE-IDENTICAL || echo DIFFER)"
