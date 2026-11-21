#!/bin/bash
# Orchestrator acceptance gate for one chunk, run inside the chunk's worktree after Neon's
# last pass has been committed. Usage: accept.sh <chNN> <code-rev-before-last-neon-pass> [chapter-dir]
# Checks: comments-only since <rev>, prose provenance, gofmt/build/vet/test, darwin cross-build,
# sources/spans/inline, evidence reproduces on 2 runs, adversarial suite, 8 sections + 5 questions,
# no machine paths. Prints one PASS/FAIL line per gate and a final ACCEPT-OK / ACCEPT-FAIL.
ch=$1; rev=$2; chdir=$3; fail=0
export SQLITE_SRC=${SQLITE_SRC:-/root/Software/sqlite-src/3.53.4}
gate() { name=$1; shift; if out=$("$@" 2>&1); then echo "PASS $name"; else echo "FAIL $name"; echo "$out" | tail -15 | sed 's/^/    /'; fail=1; fi; }
gate "tree clean" test -z "$(git status --porcelain)"
gate "comments-only since $rev (course code; adversarial scripts are Neon's own)" bash -c "go run ./cmd/course check comments $rev HEAD 2>&1 | grep 'code changed\|added between\|removed' | grep -v 'agent_work/[a-z0-9]*/adversarial/' | grep . && exit 1 || exit 0"
gate "prose provenance" go run ./cmd/course check prose a7bb640..HEAD
gate "crossbuild (linux build/vet/test, darwin build/vet, gofmt)" bash agent_work/verify/crossbuild.sh
gate "check sources" go run ./cmd/course check sources
gate "check spans" go run ./cmd/course check spans
gate "no machine paths in tracked reader-facing files" bash -c "! git ls-files | grep -v '^agent_work/\|^kb/\|^STATE.md$' | xargs grep -Il '/root/' | grep ."
if [ -n "$chdir" ]; then
  md=$chdir/$chdir.md
  gate "inline -check" go run ./cmd/course inline -check "$md"
  gate "evidence x2 ${chdir%%-*}" go run ./cmd/course check evidence "${chdir%%-*}" -runs 2
  gate "eight sections" bash -c "[ \$(grep -cE '^## [1-8]\. ' $md) -eq 8 ]"
  gate "answers file" test -s "$chdir/answers.md"
  gate "adversarial marker filled" bash -c "grep -q 'adversarial-results' $md && [ \$(sed -n '/What attacked this chapter/,/^## 7/p' $md | wc -l) -gt 6 ]"
  gate "no /root in chapter" bash -c "! grep -rn '/root/' $chdir --include='*.md' --include='*.go' --include='*.txt'"
fi
if [ -f agent_work/$ch/adversarial/run-all.sh ]; then
  bash agent_work/$ch/adversarial/run-all.sh > /tmp/accept-adv-$ch.out 2>&1; rc=$?
  held=$(grep -c '| HELD' /tmp/accept-adv-$ch.out); broken=$(grep -c '| BROKEN' /tmp/accept-adv-$ch.out)
  echo "INFO adversarial rc=$rc held=$held broken=$broken"; [ $rc -ne 0 ] && fail=1
  git checkout -q -- agent_work/$ch/adversarial 2>/dev/null
fi
[ $fail = 0 ] && echo "ACCEPT-OK $ch $(git rev-parse --short HEAD)" || echo "ACCEPT-FAIL $ch"
