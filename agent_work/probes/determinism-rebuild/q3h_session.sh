#!/usr/bin/env bash
# Q3h: session-extension changeset of phase2.sql, applied to a copy of the base.
# Needs $SESSION (see README for the build against a session-enabled libsqlite3.a).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); SESSION=${SESSION:-/tmp/sqlite-research-determinism-rebuild/session}
D=${1:-/tmp/sqlite-research-determinism-rebuild/q3h}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
{ echo "PRAGMA page_size=4096;"; cat "$HERE/seed.sql"; } | sqlite3 base.db >/dev/null
cp base.db a.db; cp base.db b.db
"$SESSION" a.db "$HERE/phase2.sql" b.db cs.bin
lh() { sqlite3 "$1" .sha3sum | cut -c1-16; }
echo "A (ran SQL) vs B (applied changeset): bytes $(cmp -s a.db b.db && echo BYTE-IDENTICAL || echo DIFFER); logical sha3 A=$(lh a.db) B=$(lh b.db)"
python3 -I "$HERE/pagediff.py" a.db b.db --ignore-header | tail -1
sqlite3 a.db "VACUUM INTO 'a_v.db'"; sqlite3 b.db "VACUUM INTO 'b_v.db'"
echo "VACUUM INTO(A) vs VACUUM INTO(B): $(cmp -s a_v.db b_v.db && echo BYTE-IDENTICAL || echo DIFFER)"
