#!/bin/bash
# Land an accepted chunk: merge its branch into main, publish knowledge to both stores, update
# STATE.md's chunk row and the INDEX per-chunk row, append the progress line, commit, push.
# Usage: land.sh <chNN> "<row label>" "<what it built>" "<review summary>" "<adversarial summary>" "<kb title>" "<kb summary>" "<kb tags>"
set -e
ch=$1; label=$2; built=$3; review=$4; adv=$5; ktitle=$6; ksum=$7; ktags=$8
repo=$(cd "$(dirname "$0")/../.." && pwd); cd "$repo"
test -z "$(git status --porcelain)" || { echo "main checkout not clean"; exit 1; }
git merge -q --no-ff chunk/$ch -m "Merge $label: $built

Review: $review. Adversarial: $adv.

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
sha=$(git rev-parse --short HEAD)
agent_work/orch/knowledge.sh "$ch" "sqlite-internals-$ch-findings" "$ktitle" "$ksum" "$ktags"
r2=""; [ -f agent_work/$ch/REVIEW-2.md ] && r2=" → [REVIEW-2]($ch/REVIEW-2.md)"
echo "| $label | [$ch/INDEX.md]($ch/INDEX.md) | [REVIEW-1]($ch/REVIEW-1.md)$r2: $review | [RESULTS]($ch/adversarial/RESULTS.md): $adv |" >> agent_work/INDEX.md
LABEL="$label" REVIEW="$review" ADV="$adv" SHA="$sha" python3 - <<'PY'
import os,re
s=open('STATE.md').read(); lab=os.environ['LABEL']
row=f"| {lab} | done | {os.environ['REVIEW']} | {os.environ['ADV']} | merged {os.environ['SHA']} |"
key=lab.split()[0]
lines=s.split('\n'); out=[]; done=False
for l in lines:
    if l.startswith(f"| {key} ") and not done: out.append(row); done=True
    else: out.append(l)
if not done:
    i=max(i for i,l in enumerate(out) if l.startswith('| ') and 'merged' in l); out.insert(i+1,row)
open('STATE.md','w').write('\n'.join(out))
PY
git add -A; git commit -qm "state: $label landed ($sha); knowledge entry and index row

Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
git push -q origin main
echo "$(date -Is) $label: $built; review: $review; adversarial: $adv; merged $sha and pushed" >> /root/runs/sqlite-phase2-progress.txt
echo "LANDED $ch $sha"
