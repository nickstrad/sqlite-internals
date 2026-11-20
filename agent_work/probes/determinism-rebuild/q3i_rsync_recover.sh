#!/usr/bin/env bash
# Q3i: sqlite3_rsync (local->local) and .recover on a damaged copy.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); BLD=${BLD:-/tmp/sqlite-research-determinism-rebuild/bld}
D=${1:-/tmp/sqlite-research-determinism-rebuild/q3i}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
{ echo "PRAGMA journal_mode=WAL;"; cat "$HERE/seed.sql"; } | sqlite3 o.db >/dev/null
$BLD/sqlite3_rsync o.db r.db -v 2>&1 | tail -3; echo "rsync exit=$?"
sqlite3 o.db "PRAGMA wal_checkpoint(TRUNCATE)" >/dev/null; sqlite3 r.db "PRAGMA wal_checkpoint(TRUNCATE)" >/dev/null
echo "replica vs origin (both checkpointed): $(cmp -s o.db r.db && echo BYTE-IDENTICAL || echo DIFFER) ; journal_mode replica=$(sqlite3 r.db 'pragma journal_mode')"
python3 -I "$HERE/pagediff.py" o.db r.db | tail -2
sqlite3 o.db "UPDATE orders SET amount=amount+1 WHERE id<50;" ; $BLD/sqlite3_rsync o.db r.db -v 2>&1 | tail -2
sqlite3 o.db "PRAGMA wal_checkpoint(TRUNCATE)" >/dev/null; sqlite3 r.db "PRAGMA wal_checkpoint(TRUNCATE)" >/dev/null
echo "after incremental sync: $(cmp -s o.db r.db && echo BYTE-IDENTICAL || echo DIFFER)"
echo "--- .recover on a damaged db (zero 4 pages):"
cp o.db dmg.db; for p in 20 40 60 80; do dd if=/dev/zero of=dmg.db bs=4096 seek=$((p-1)) count=1 conv=notrunc status=none; done
echo "integrity: $(sqlite3 dmg.db 'pragma integrity_check' 2>&1 | head -1)"
sqlite3 dmg.db .recover > rec.sql 2>/dev/null; sqlite3 rec.db < rec.sql 2>/dev/null
echo "orders rows original=$(sqlite3 o.db 'select count(*) from orders') recovered=$(sqlite3 rec.db 'select count(*) from orders') lost_and_found tables: $(sqlite3 rec.db "select group_concat(name) from sqlite_schema where name like 'lost%'")"
