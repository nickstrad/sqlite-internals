#!/bin/bash
# Render and launch the three first-round Neon runs for a built chunk: prose pass, code review,
# adversarial tests. Usage: neon-trio.sh <chNN> "<title>" <chapter-dir> "<outline ref>" "<code paths>" <base-rev> ["<prose note>"]
ch=$1; export CH=$1 TITLE=$2 CHDIR=$3 OUTLINE_REF=$4 CODE_PATHS=$5 BASE=$6
export WT=/root/Software/sqlite-internals-work/$ch ROUND="" ROUND_NOTE="" REVIEW_FILE=REVIEW-1.md
here=$(cd "$(dirname "$0")" && pwd); b=/root/briefs/sqlite-p2
$here/mkbrief.sh neon-prose $ch-prose >/dev/null; [ -n "$7" ] && printf '\nChunk note: %s\n' "$7" >> $b/$ch-prose.md
$here/mkbrief.sh neon-review $ch-review1 >/dev/null; $here/mkbrief.sh neon-adversarial $ch-adversarial >/dev/null
mkdir -p $WT/agent_work/$ch/adversarial
for s in prose review1 adversarial; do $here/launch.sh $ch-$s $WT neon $b/$ch-$s.md; done
