#!/usr/bin/env bash
# Q3 a,b,d,e,g: capture methods on a quiescent source, compared to the source file.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
D=${1:-/tmp/sqlite-research-determinism-rebuild/q3}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
eq() { cmp -s "$1" "$2" && echo BYTE-IDENTICAL || echo "DIFFER($(python3 -I $HERE/pagediff.py "$1" "$2" | tail -1 | sed 's/differing pages: //') pages; ignoring hdr: $(python3 -I $HERE/pagediff.py "$1" "$2" --ignore-header | tail -1 | sed 's/differing pages: //'))"; }
lh() { sqlite3 "$1" .sha3sum | cut -c1-16; }
# source: rollback-journal db built from seed + phase2 (has freelist pages)
{ echo "PRAGMA page_size=4096;"; cat "$HERE/seed.sql"; cat "$HERE/phase2.sql"; } | sqlite3 src.db >/dev/null
echo "source: $(stat -c %s src.db) bytes, logical sha3=$(lh src.db), freelist=$(sqlite3 src.db 'pragma freelist_count')"
# a. plain copy (quiescent)
cp src.db a_copy.db; echo "a. cp quiescent: $(eq src.db a_copy.db)"
# b. sqlite_dbpage inside one read transaction, pages concatenated in pgno order
mkdir -p pg; sqlite3 src.db "BEGIN; SELECT count(writefile(printf('pg/%08d',pgno), data)) FROM sqlite_dbpage; COMMIT;" >/dev/null
cat pg/* > b_dbpage.db; echo "b. sqlite_dbpage: $(eq src.db b_dbpage.db)"
# d. .backup
sqlite3 src.db ".backup d_backup.db"; echo "d. .backup: $(eq src.db d_backup.db)"; python3 -I $HERE/pagediff.py src.db d_backup.db | sed -n 2p
python3 -I $HERE/hdr.py src.db d_backup.db | sed 's/default_cache.*version_valid/... version_valid/'
# e. VACUUM INTO (twice), vs source
sqlite3 src.db "VACUUM INTO 'e_vac1.db'"; sqlite3 src.db "VACUUM INTO 'e_vac2.db'"
echo "e. VACUUM INTO vs source: $(eq src.db e_vac1.db); run1 vs run2: $(eq e_vac1.db e_vac2.db); logical equal: $([ $(lh src.db) = $(lh e_vac1.db) ] && echo yes || echo no)"
# e2. VACUUM INTO as canonicalizer: three different physical builds of the same logical content
bash "$HERE/q1_build_determinism.sh" "$D/q1" >/dev/null 2>&1 || true
for f in batch_onetxn batch_autocommit batch_rowwise; do sqlite3 q1/$f.db "VACUUM INTO 'canon_$f.db'"; done
echo "e2. raw: onetxn vs autocommit: $(eq q1/batch_onetxn.db q1/batch_autocommit.db); onetxn vs rowwise: $(eq q1/batch_onetxn.db q1/batch_rowwise.db)"
echo "    VACUUM INTO: onetxn vs autocommit: $(eq canon_batch_onetxn.db canon_batch_autocommit.db); onetxn vs rowwise: $(eq canon_batch_onetxn.db canon_batch_rowwise.db)"
# e3. VACUUM INTO vs from-scratch rebuild in a single transaction with no deletes (q1 'rowwise' style is already that)
# g. .dump -> replay
sqlite3 src.db .dump > dump.sql
sqlite3 g_replay1.db < dump.sql; sqlite3 g_replay2.db < dump.sql
echo "g. .dump replay vs source: $(eq src.db g_replay1.db); replay1 vs replay2: $(eq g_replay1.db g_replay2.db); logical equal: $([ $(lh src.db) = $(lh g_replay1.db) ] && echo yes || echo no)"
echo "   replay vs VACUUM INTO of source: $(eq g_replay1.db e_vac1.db)"
sqlite3 g_replay1.db "VACUUM INTO 'g_replay_vac.db'"; echo "   VACUUM INTO(replay) vs VACUUM INTO(source): $(eq g_replay_vac.db e_vac1.db)"
head -c 300 dump.sql | head -4
