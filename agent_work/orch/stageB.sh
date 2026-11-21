#!/bin/bash
# Chain, part B: the closing Neon prose pass and the acceptance gate.
# Usage: stageB.sh <chNN> "<title>" <chapter-dir|-> "<outline ref>" "<code paths>" <mode: late|closing> ["<note>"]
#   late    = Neon writes the chapter and all comments now (single prose pass, new ordering)
#   closing = a chapter draft exists (old ordering): Neon's closing rewrite
# The branch must already contain main. Ends by writing /root/runs/sqlite-p2-<chNN>-accept.txt.
. "$(dirname "$0")/lib.sh"
ch=$1; title=$2; chdir=$3; oref=$4; paths=$5; mode=$6; note=$7; [ "$chdir" = - ] && chdir=""
wt=/root/Software/sqlite-internals-work/$ch; cd $wt || exit 1
acc=$RUNS/sqlite-p2-$ch-accept.txt; rm -f $acc
test -z "$(git status --porcelain)" || { echo "ACCEPT-FAIL $ch worktree not clean before prose" > $acc; exit 1; }
pre=$(git rev-parse --short HEAD); echo $pre > $RUNS/sqlite-p2-$ch.preprose
export CH=$ch TITLE="$title" OUTLINE_REF="$oref" CODE_PATHS="$paths" WT=$wt CHDIR="$chdir" BASE="" LATE_NOTE="$note" CLOSING_NOTE="$note"
if [ $mode = late ]; then
  $ORCH/mkbrief.sh neon-prose $ch-prose >/dev/null
  envsubst '${CH} ${LATE_NOTE}' < $BR/tmpl-neon-late-section.md >> $BR/$ch-prose.md
  msg="$ch: write the chapter text and comment the code (closing prose pass)"
else
  $ORCH/mkbrief.sh neon-closing $ch-prose >/dev/null
  msg="$ch: closing rewrite of the chapter copy"
fi
runstep $ch-prose $wt neon $BR/$ch-prose.md 100 || say "prose run did not end cleanly; the gate decides"
git add -A && git commit -q --author="$NA" -m "$msg"
$ORCH/accept.sh $ch $pre $chdir 2>&1 | grep -aE "^(PASS|FAIL|INFO|ACCEPT|    )" | cut -c1-240 > $acc.tmp; mv $acc.tmp $acc
cat $acc
