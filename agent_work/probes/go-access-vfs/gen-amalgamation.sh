#!/usr/bin/env bash
# Build sqlite3.c/sqlite3.h from the pinned tree (in a scratch copy), check them against the
# official sqlite-amalgamation-3530400.zip, and drop them into cgo/amalg/ and vfstrace.c into cgo/sqlitec/ (gitignored).
set -euo pipefail
PIN=/root/Software/sqlite-src/3.53.4
SCRATCH=${SCRATCH:-/tmp/sqlite-research-go-access-vfs}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$SCRATCH"
rm -rf "$SCRATCH/tree" && cp -a "$PIN" "$SCRATCH/tree"
( cd "$SCRATCH/tree" && ./configure >"$SCRATCH/configure.log" 2>&1 && make sqlite3.c >"$SCRATCH/make.log" 2>&1 )
sha256sum "$SCRATCH/tree/sqlite3.c" "$SCRATCH/tree/sqlite3.h"
if [ "${CHECK_OFFICIAL:-1}" = 1 ]; then
  mkdir -p "$SCRATCH/official" && cd "$SCRATCH/official"
  [ -f a.zip ] || curl -sSfo a.zip https://www.sqlite.org/2026/sqlite-amalgamation-3530400.zip
  unzip -oq a.zip
  cmp "$SCRATCH/official/sqlite-amalgamation-3530400/sqlite3.c" "$SCRATCH/tree/sqlite3.c" && echo "sqlite3.c identical to official zip"
  cmp "$SCRATCH/official/sqlite-amalgamation-3530400/sqlite3.h" "$SCRATCH/tree/sqlite3.h" && echo "sqlite3.h identical to official zip"
fi
cp "$SCRATCH/tree/sqlite3.c" "$SCRATCH/tree/sqlite3.h" "$HERE/cgo/amalg/"
cp "$SCRATCH/tree/ext/misc/vfstrace.c" "$HERE/cgo/sqlitec/vfstrace.c.inc"
echo "copied into $HERE/cgo/sqlitec/"
