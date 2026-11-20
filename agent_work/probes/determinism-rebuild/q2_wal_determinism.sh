#!/usr/bin/env bash
# Q2 + Q3c: WAL byte-determinism, frame-tuple determinism, and "replay WAL frames yourself".
# For each config, run twice: seed -> TRUNCATE checkpoint -> copy base -> phase2 with
# wal_autocheckpoint=0 -> copy -wal while open -> TRUNCATE checkpoint -> copy result.
# Needs: walreplay binary at $WALREPLAY (build: cd go && go build -o $WALREPLAY ./walreplay)
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
D=${1:-/tmp/sqlite-research-determinism-rebuild/q2}; rm -rf "$D"; mkdir -p "$D"; cd "$D"
WR=${WALREPLAY:-/tmp/sqlite-research-determinism-rebuild/walreplay}
run() { # tag run# extra-pragmas [ckpt-threshold]
  local t=$1 r=$2 extra=$3 ac=${4:-0} p=$1.$2
  { echo "$extra"; echo "PRAGMA page_size=4096; PRAGMA journal_mode=WAL;"
    cat "$HERE/seed.sql"; echo "PRAGMA wal_checkpoint(TRUNCATE);"
    echo ".shell cp $p.db $p.base"
    echo "PRAGMA wal_autocheckpoint=$ac;"
    cat "$HERE/phase2.sql"
    echo ".shell cp $p.db-wal $p.wal"; echo ".shell cp $p.db $p.pre"
    echo "PRAGMA wal_checkpoint(TRUNCATE);"
    echo ".shell cp $p.db $p.ckpt"; } | sqlite3 $p.db >/dev/null
  "$WR" frames $p.wal > $p.frames
  # frame tuples without the header line (salts) and frame index
  grep '^frame' $p.frames | awk '{print $3,$4,$5}' > $p.tuples
}
cmp_cfg() { local t=$1
  printf '%-14s wal-bytes:%s  tuples:%s  frames=%s  header1:%s\n' $t \
    "$(cmp -s $t.1.wal $t.2.wal && echo IDENTICAL || echo DIFFER)" \
    "$(cmp -s $t.1.tuples $t.2.tuples && echo IDENTICAL || echo DIFFER)" \
    "$(wc -l < $t.1.tuples)" "$(head -1 $t.1.frames | cut -d' ' -f4-)"
  printf '%-14s header2:%s\n' "" "$(head -1 $t.2.frames | cut -d' ' -f4-)"
  "$WR" apply $t.1.base $t.1.wal $t.1.replayed >/dev/null
  printf '%-14s replay(base,wal) vs SQLite checkpoint: %s ; final dbs run1 vs run2: %s\n' "" \
    "$(cmp -s $t.1.replayed $t.1.ckpt && echo BYTE-IDENTICAL || echo DIFFER)" \
    "$(cmp -s $t.1.ckpt $t.2.ckpt && echo BYTE-IDENTICAL || echo DIFFER)"
}
for r in 1 2; do run full $r "PRAGMA synchronous=FULL;"; done; cmp_cfg full
for r in 1 2; do run normal $r "PRAGMA synchronous=NORMAL;"; done; cmp_cfg normal
for r in 1 2; do run off $r "PRAGMA synchronous=OFF;"; done; cmp_cfg off
for r in 1 2; do run seeded $r ".testctrl prng_seed 12345"; done; cmp_cfg seeded
echo "--- tuples across synchronous settings: full vs normal: $(cmp -s full.1.tuples normal.1.tuples && echo IDENTICAL || echo DIFFER); full vs off: $(cmp -s full.1.tuples off.1.tuples && echo IDENTICAL || echo DIFFER)"
echo "--- wal bytes full vs normal sizes: $(stat -c %s full.1.wal) $(stat -c %s normal.1.wal)"
echo "--- byte diff of the two full-sync WALs (offset, run1, run2 octal), first 6:"; cmp -l full.1.wal full.2.wal | head -6 || true; echo "total differing bytes: $(cmp -l full.1.wal full.2.wal | wc -l)"
echo "--- differing byte offsets classified (all must be in WAL header 16..31 or frame-header salt/cksum fields):"
cmp -l full.1.wal full.2.wal | awk '{o=$1-1; if(o<32){k="walhdr:" (o<24?"salt":"cksum")} else {r=(o-32)%(24+4096); k=(r<8?"frame-pgno/commit":(r<16?"frame-salt":(r<24?"frame-cksum":"PAGE-DATA")))} c[k]++} END{for(k in c) print "  ",k,c[k]}'  || true
# autocheckpoint threshold 100: WAL restarts mid-workload (salt1 incremented, new salt2),
# old-generation frames remain past the new ones; replay must start from the db as it was
# when the WAL was copied (.pre), not from the original base.
for r in 1 2; do run ac100 $r "" 100; done
printf 'ac100          tuples run1 vs run2: %s ; frames valid=%s; %s\n' "$(cmp -s ac100.1.tuples ac100.2.tuples && echo IDENTICAL || echo DIFFER)" "$(wc -l < ac100.1.tuples)" "$(tail -1 ac100.1.frames)"
echo "ac100 header: $(head -1 ac100.1.frames)"
echo "ac100 tuples vs full (autockpt off): $(cmp -s ac100.1.tuples full.1.tuples && echo IDENTICAL || echo DIFFER)"
"$WR" apply ac100.1.pre ac100.1.wal ac100.1.replayed
echo "ac100 replay(pre,wal) vs ckpt: $(cmp -s ac100.1.replayed ac100.1.ckpt && echo BYTE-IDENTICAL || echo DIFFER); replay(base,wal) vs ckpt: $("$WR" apply ac100.1.base ac100.1.wal ac100.1.bad >/dev/null; cmp -s ac100.1.bad ac100.1.ckpt && echo BYTE-IDENTICAL || echo DIFFER)"
sqlite3 ac100.1.bad "PRAGMA integrity_check;" 2>&1 | head -3 || true
