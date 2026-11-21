#!/bin/bash
# Render a Neon brief from a template in /root/briefs/sqlite-p2/ with envsubst.
# Usage: CH=ch01 TITLE=... WT=... [other vars] mkbrief.sh <template-name> <out-name>
# Writes /root/briefs/sqlite-p2/<out-name>.md and prints its path.
d=/root/briefs/sqlite-p2
if [ -n "$CHDIR" ]; then export CHAPTER_SECTION="$(envsubst < $d/tmpl-chapter-section.md)"; else export CHAPTER_SECTION=""; fi
vars='${CH} ${TITLE} ${WT} ${OUTLINE_REF} ${CODE_PATHS} ${CHAPTER_SECTION} ${CHDIR} ${BASE} ${ROUND} ${ROUND_NOTE} ${REVIEW_FILE} ${PROSE_REV} ${FINALIZE_EXTRA} ${REPAIR_EXTRA} ${CHAPTER_TOUCHUP} ${LATE_NOTE} ${CLOSING_NOTE}'
envsubst "$vars" < "$d/tmpl-$1.md" > "$d/$2.md"; echo "$d/$2.md"
