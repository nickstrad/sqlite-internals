#!/usr/bin/env bash
# Q5: read the wal-index header (-shm) from outside SQLite while a connection is open:
# WalIndexHdr (48 bytes, two copies at 0 and 48; native byte order) and WalCkptInfo at 96.
# Shows mxFrame growing with commits and nBackfill after a PASSIVE checkpoint, and that a
# held read transaction in another process caps the checkpoint (mxSafeFrame = its read mark).
set -uo pipefail
D=${1:-/tmp/sqlite-research-determinism-rebuild/q5}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
cat > peek.py <<'PY'
import struct, sys
b = open(sys.argv[1], 'rb').read(136)
iVersion, _, iChange, isInit, bigEnd, szPage, mxFrame, nPage = struct.unpack('<IIIBBHII', b[:24])
salt1, salt2 = struct.unpack('<II', b[32:40])
nBackfill = struct.unpack('<I', b[96:100])[0]; marks = struct.unpack('<5I', b[100:120])
print(f"  shm: iChange={iChange} mxFrame={mxFrame} nPage={nPage} nBackfill={nBackfill} readMarks={[hex(m) if m==0xffffffff else m for m in marks]}")
PY
sqlite3 s.db "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE t(a);" >/dev/null
# reader process: open read txn after 2 commits, hold it 3 s
( sleep 0.5; sqlite3 s.db "BEGIN; SELECT count(*) FROM t;" ".shell sleep 3" "COMMIT;" >/dev/null ) &
sqlite3 s.db "PRAGMA wal_autocheckpoint=0;" "INSERT INTO t VALUES(1);" "INSERT INTO t VALUES(2);" ".shell python3 -I peek.py s.db-shm" \
  ".shell sleep 1" "INSERT INTO t VALUES(3);" "INSERT INTO t VALUES(4);" ".shell python3 -I peek.py s.db-shm" \
  ".print checkpoint PASSIVE while reader holds snapshot (busy|log|ckpt):" "PRAGMA wal_checkpoint(PASSIVE);" ".shell python3 -I peek.py s.db-shm" \
  ".shell sleep 3" ".print checkpoint PASSIVE after reader finished (busy|log|ckpt):" "PRAGMA wal_checkpoint(PASSIVE);" ".shell python3 -I peek.py s.db-shm"
wait
