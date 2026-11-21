#!/bin/bash
# Publish one landed chunk's durable findings to both knowledge stores (Nick's ruling 6).
# Usage: knowledge.sh <chNN> <slug> "<title>" "<summary>" "<tags, comma separated>"
# Builds docs/knowledge/<slug>.md from the chunk's build notes ("Corrections to the outline"
# section and the build report's corrections list), re-indexes the in-repo kb location, and
# adds/refreshes the same entry in the vm-1 kb store. Each numbered correction gets its own
# heading so that no indexed section exceeds the embedding model's 2048-token input limit.
ch=$1; slug=$2; title=$3; summary=$4; tags=$5
repo=$(cd "$(dirname "$0")/../.." && pwd); out=$repo/docs/knowledge/$slug.md
today=$(date +%F)
{
  printf -- '---\ntitle: "%s"\nsummary: "%s"\ntags: [%s]\nupdated: %s\nverified: %s — Linux amd64, Go 1.26, SQLite 3.53.4 via modernc.org/sqlite v1.60.1; evidence in the sqlite-internals repo\n---\n\n' "$title" "$summary" "$tags" "$today" "$today"
  printf '# %s\n\n%s\n\nSource: the `sqlite-internals` course repo (github.com/nickstrad/sqlite-internals), chunk `%s`. Working notes: `agent_work/%s/build-notes.md`; review and adversarial records in the same directory.\n\n' "$title" "$summary" "$ch" "$ch"
  awk '/^## Corrections to the outline/{p=1} p&&/^## /&&!/Corrections to the outline/{p=0} p' "$repo/agent_work/$ch/build-notes.md" | awk '/^[0-9]+\. /{n=$1; sub(/\.$/,"",n); print "\n### Correction " n "\n"} {print}'
  [ -f "$repo/agent_work/$ch/knowledge-extra.md" ] && cat "$repo/agent_work/$ch/knowledge-extra.md"
} | sed 's|/root/Software/sqlite-src/3.53.4|$SQLITE_SRC|g; s|/root/Software/sqlite-internals[-a-z0-9/]*/||g' > "$out"
grep -n "/root/" "$out" && echo "WARNING: machine path left in $out"
"$repo/agent_work/kb-sync.sh" index | tail -1
if [ -f /root/Raw/knowledge/data/$slug.md ]; then cp "$out" /root/Raw/knowledge/data/$slug.md; (cd /root/Raw/knowledge && kb reindex 2>&1 | tail -1)
else (cd /root/Raw/knowledge && kb add "$out" 2>&1 | tail -1); fi
