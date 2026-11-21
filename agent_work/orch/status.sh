#!/bin/bash
# One line per chunk: where its chain stands (part A result, part B gate result, last log line).
R=/root/runs
for c in ch01 ch02 ch03 ch04 ch05 ch06 ch07 ch08 ch09 ch10a ch10b pub; do
  a=$(cat $R/sqlite-p2-$c.stageA 2>/dev/null | cut -c1-150); g=$(tail -n 1 $R/sqlite-p2-$c-accept.txt 2>/dev/null)
  f=$(grep -c '^FAIL' $R/sqlite-p2-$c-accept.txt 2>/dev/null)
  l=$(tail -n 1 $R/sqlite-p2-$c-chainA.log 2>/dev/null | cut -c12-130); lb=$(tail -n 1 $R/sqlite-p2-$c-chainB.log 2>/dev/null | cut -c1-90)
  run=$(for p in $R/sqlite-p2-$c-*.pid; do n=${p%.pid}; [ -f $n.exit ] || { kill -0 $(cat $p) 2>/dev/null && basename $n | sed 's/sqlite-p2-//'; }; done 2>/dev/null | tr '\n' ' ')
  echo "$c | A: ${a:-$l} | B: ${g:-$lb} fails=$f | running: $run"
done
