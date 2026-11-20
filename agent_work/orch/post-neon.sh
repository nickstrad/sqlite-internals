#!/bin/bash
# After a chunk's first Neon round (prose, review, adversarial): commit each under Neon's name,
# verify the prose pass changed comments only since the Opus build, run the adversarial suite
# once and keep its raw output as results-run1.txt. Usage: post-neon.sh <chNN> <build-rev> [chapter-dir]
ch=$1; rev=$2; chdir=$3; cd /root/Software/sqlite-internals-work/$ch || exit 1
export SQLITE_SRC=/root/Software/sqlite-src/3.53.4
NA='Gemini 4 Neon (via OpenCode) <noreply@google.com>'
git add -A -- ':!agent_work' ; git add agent_work/$ch/neon-prose-notes.md 2>/dev/null
git commit -q --author="$NA" -m "$ch: write the chapter text and comment the code (prose pass)"
prose=$(git rev-parse --short HEAD)
git add agent_work/$ch/REVIEW-1.md && git commit -q --author="$NA" -m "$ch: code review round 1 ($(head -1 agent_work/$ch/REVIEW-1.md))"
git add agent_work/$ch/adversarial && git commit -q --author="$NA" -m "$ch: adversarial attacks"
git status --short | head -5
echo "prose commit $prose; $(head -1 agent_work/$ch/REVIEW-1.md)"
go run ./cmd/course check comments $rev $prose 2>&1 | tail -3
gofmt -l . 2>/dev/null | grep -v '^kb/' | head -3; go build ./... 2>&1 | head -5
[ -n "$chdir" ] && { go run ./cmd/course inline -check $chdir/$chdir.md 2>&1 | tail -1; wc -l $chdir/$chdir.md; }
grep -nE "^(#+ |[0-9]+\. )?.*\b(blocker|should-fix)\b" agent_work/$ch/REVIEW-1.md | cut -c1-210 | head -14
( time bash agent_work/$ch/adversarial/run-all.sh > /tmp/adv-$ch-1.out 2>&1 ) 2>&1 | grep real; echo "adv rc=$?"
cp /tmp/adv-$ch-1.out agent_work/$ch/adversarial/results-run1.txt; git checkout -q -- agent_work/$ch/adversarial/results.txt 2>/dev/null
echo "held=$(grep -c '| HELD' /tmp/adv-$ch-1.out) broken=$(grep -c '| BROKEN' /tmp/adv-$ch-1.out)"
grep '| BROKEN' /tmp/adv-$ch-1.out | cut -c1-330
