#!/usr/bin/env bash
set -u
src=${SQLITE_SRC:?set SQLITE_SRC to the pinned tree}
while read -r file start symbol; do
  [ -z "$file" ] && continue
  end=$(awk -v s="$start" 'NR>=s && /^}/ {print NR; exit}' "$src/$file")
  echo "$file $start $end $symbol"
done
