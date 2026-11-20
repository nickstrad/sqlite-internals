#!/usr/bin/env bash
# Q1b: one-transaction vs autocommit builds of seed.sql differ only in header bytes + stale
# content of a freelist leaf page; PRAGMA secure_delete=ON zeroes it and the pages then match.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); D=${1:-/tmp/sqlite-research-determinism-rebuild/q1b}
rm -rf "$D"; mkdir -p "$D"; cd "$D"
for sd in 0 1; do
  { echo "PRAGMA secure_delete=$sd; BEGIN;"; cat "$HERE/seed.sql"; echo "COMMIT;"; } | sqlite3 one$sd.db >/dev/null
  { echo "PRAGMA secure_delete=$sd;"; cat "$HERE/seed.sql"; } | sqlite3 auto$sd.db >/dev/null
  echo "== secure_delete=$sd: freelist leaves:"; python3 -I - one$sd.db <<'PY'
import struct,sys
d=open(sys.argv[1],'rb').read(); ps=struct.unpack('>H',d[16:18])[0]; t=struct.unpack('>I',d[32:36])[0]
while t:
    p=d[(t-1)*ps:t*ps]; n=struct.unpack('>I',p[4:8])[0]
    print(' trunk',t,'leaves',[struct.unpack('>I',p[8+4*i:12+4*i])[0] for i in range(n)]); t=struct.unpack('>I',p[0:4])[0]
PY
  python3 -I "$HERE/pagediff.py" one$sd.db auto$sd.db --ignore-header || true
done
