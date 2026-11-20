#!/usr/bin/env bash
# Fetch the pinned SQLite source tree (tag version-3.53.4) and verify it is the
# same revision as the sqlite3 CLI installed on this machine.
# Usage: agent_work/fetch-pinned-sqlite.sh [dest-dir]   (default /root/Software/sqlite-src/3.53.4)
set -euo pipefail
TAG=version-3.53.4
GIT_SHA=b09c88c14082339b66c7b7158d609a771e64ca69
FOSSIL_ID=bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc
DEST=${1:-/root/Software/sqlite-src/3.53.4}
if [ ! -d "$DEST/.git" ]; then
  mkdir -p "$(dirname "$DEST")"
  git clone -q --depth 1 --branch "$TAG" https://github.com/sqlite/sqlite "$DEST"
fi
got_sha=$(git -C "$DEST" rev-parse HEAD)
got_id=$(cat "$DEST/manifest.uuid")
[ "$got_sha" = "$GIT_SHA" ] || { echo "FAIL git sha $got_sha != $GIT_SHA"; exit 1; }
[ "$got_id" = "$FOSSIL_ID" ] || { echo "FAIL manifest.uuid $got_id != $FOSSIL_ID"; exit 1; }
if command -v sqlite3 >/dev/null; then
  cli=$(sqlite3 :memory: 'select sqlite_source_id()' | awk '{print $3}')
  [ "$cli" = "$FOSSIL_ID" ] && echo "OK installed sqlite3 CLI matches pin" || echo "WARN installed sqlite3 CLI source id $cli differs from pin"
fi
echo "OK $DEST at $TAG git=$GIT_SHA fossil=$FOSSIL_ID"
