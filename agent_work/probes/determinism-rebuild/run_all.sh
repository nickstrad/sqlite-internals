#!/usr/bin/env bash
# Build the Go helpers and the pinned-tree tools, then run every probe in order.
# Output of each probe goes to $OUT/<probe>.out. Scratch under /tmp/sqlite-research-determinism-rebuild.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); R=/tmp/sqlite-research-determinism-rebuild; OUT=${OUT:-$R/out}; mkdir -p "$OUT"
export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false
export BLD=$R/bld WALREPLAY=$R/walreplay SER=$R/serialize SESSION=$R/session
if [ ! -x "$BLD/sqldiff" ]; then
  rm -rf $R/src $BLD; cp -r /root/Software/sqlite-src/3.53.4 $R/src; mkdir -p $BLD
  (cd $BLD && ../src/configure --enable-all >cfg.log 2>&1 && make -j8 sqlite3.c libsqlite3.a dbhash sqldiff sqlite3_rsync >mk.log 2>&1)
fi
(cd "$HERE/go" && go build -o $WALREPLAY ./walreplay && go build -o $SER ./serialize && \
  CGO_CFLAGS="-I$BLD" CGO_LDFLAGS="-L$BLD" go build -o $SESSION ./session)
for p in q1_build_determinism q1b_freelist_secure_delete q1c_sql_nondeterminism q2_wal_determinism q3c_wal_tail \
         q3_capture_methods q3_concurrent q3f_serialize q3h_session q3i_rsync_recover q4_verification_ladder q5_shm_peek q6_kill9; do
  echo "== $p"; bash "$HERE/$p.sh" > "$OUT/$p.out" 2>&1; echo "   exit=$? -> $OUT/$p.out"
done
(cd "$HERE/go" && go run ./modernc_build ../seed.sql $R/modernc.db) > "$OUT/modernc.out" 2>&1
echo "modernc vs CLI delete-mode build: $(cmp -s $R/modernc.db $R/q1/delete.1.db && echo BYTE-IDENTICAL || echo DIFFER)" >> "$OUT/modernc.out"
