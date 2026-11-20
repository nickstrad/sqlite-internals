#!/bin/bash
# Keep the two knowledge stores and the vendored kb tool in sync (Nick's ruling 6).
#   kb-sync.sh vendor   re-copy /root/Software/kb (tracked files at HEAD) into kb/, record the SHA
#   kb-sync.sh index    rebuild the in-repo index over docs/knowledge/*.md with the vendored kb
#   kb-sync.sh status   show vendored SHA vs upstream HEAD, and entry counts
# The vm-1 store itself is updated with the installed `kb` CLI (kb add / kb edit), one entry per
# landed chunk, mirroring the docs/knowledge/ file of the same name.
repo=$(cd "$(dirname "$0")/.." && pwd); up=/root/Software/kb
case "$1" in
  vendor)
    rm -rf "$repo/kb"; mkdir "$repo/kb"
    git -C "$up" archive HEAD | tar -x -C "$repo/kb"
    git -C "$up" rev-parse HEAD > "$repo/agent_work/kb-vendor.sha"
    echo "vendored kb @ $(cat "$repo/agent_work/kb-vendor.sha")" ;;
  index)
    [ -x "$repo/_tools/bin/kb" ] || (cd "$repo/kb" && go build -tags fts5 -o "$repo/_tools/bin/kb" ./cmd/kb)
    (cd "$repo" && _tools/bin/kb reindex --all) ;;
  status)
    echo "vendored: $(cat "$repo/agent_work/kb-vendor.sha")"; echo "upstream: $(git -C "$up" rev-parse HEAD)"
    ls "$repo"/docs/knowledge/*.md | wc -l ;;
  *) echo "usage: kb-sync.sh vendor|index|status"; exit 2 ;;
esac
