#!/usr/bin/env bash
# Build the state-inspection tools (ch.9) from a COPY of the pinned SQLite tree.
# Usage: [TARGETS="..."] [REUSE=1] build-tools.sh [pinned-tree] [scratch-dir]
# Default configure (no Tcl): sqlite3_analyzer is expected to FAIL (needs tclConfig.sh).
# Never builds inside the pinned tree. Prints one line per target: OK/FAIL + binary size.
set -u
PIN=${1:-/root/Software/sqlite-src/3.53.4}
SCR=${2:-/tmp/sqlite-research-source-inventory-b}
mkdir -p "$SCR"
if [ "${REUSE:-0}" != 1 ] || [ ! -f "$SCR/src-copy/Makefile" ]; then
  rm -rf "$SCR/src-copy"; cp -a "$PIN" "$SCR/src-copy"
  (cd "$SCR/src-copy" && ./configure >"$SCR/configure.log" 2>&1) || { echo "configure FAILED (see $SCR/configure.log)"; exit 1; }
fi
cd "$SCR/src-copy"
TARGETS=${TARGETS:-"sqlite3 sqldiff dbhash sqlite3_rsync showdb showwal showjournal showshm showstat4 dbtotxt dbdump changeset sqlite3_expert sqlite3_analyzer"}
for t in $TARGETS; do
  if make -j"$(nproc)" "$t" >"$SCR/make-$t.log" 2>&1; then
    f=$t; [ -f "$f" ] || f=$(ls -1 "$t"* 2>/dev/null | head -1)
    echo "OK   $t  $(stat -c %s "$f" 2>/dev/null) bytes"
  else
    echo "FAIL $t  (tail: $(tail -1 "$SCR/make-$t.log"))"
  fi
done
