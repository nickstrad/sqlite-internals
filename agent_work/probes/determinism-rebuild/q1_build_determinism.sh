#!/usr/bin/env bash
# Q1: build the same fixed SQL script twice into fresh files under several configurations
# and compare sha256. Usage: q1_build_determinism.sh [scratchdir]
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
D=${1:-/tmp/sqlite-research-determinism-rebuild/q1}
rm -rf "$D"; mkdir -p "$D"; cd "$D"
SEED="$HERE/seed.sql"
build() { # name prelude [postlude]
  local name=$1 pre=$2 post=${3:-}
  for run in 1 2; do
    rm -f "$name.$run.db"*
    { echo "$pre"; echo "BEGIN;"; cat "$SEED"; echo "COMMIT;"; echo "$post"; } | sqlite3 "$name.$run.db" >/dev/null
  done
  local h1 h2; h1=$(sha256sum < "$name.1.db" | cut -c1-16); h2=$(sha256sum < "$name.2.db" | cut -c1-16)
  printf '%-28s run1=%s run2=%s %s size=%s\n' "$name" "$h1" "$h2" "$([ $h1 = $h2 ] && echo IDENTICAL || echo DIFFER)" "$(stat -c %s $name.1.db)"
}
build delete      "PRAGMA page_size=4096; PRAGMA journal_mode=DELETE;"
build wal         "PRAGMA page_size=4096; PRAGMA journal_mode=WAL;" "PRAGMA wal_checkpoint(TRUNCATE);"
build wal_nockpt  "PRAGMA page_size=4096; PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"
build autovac_full "PRAGMA page_size=4096; PRAGMA auto_vacuum=FULL;"
build autovac_incr "PRAGMA page_size=4096; PRAGMA auto_vacuum=INCREMENTAL;" "PRAGMA incremental_vacuum;"
build spill_cache5 "PRAGMA page_size=4096; PRAGMA cache_size=5;"
build page1024    "PRAGMA page_size=1024;"
# batching: the same logical rows, (a) one txn  (b) autocommit per statement  (c) row-at-a-time inserts
rm -f batch_*.db*
{ echo "BEGIN;"; cat "$SEED"; echo "COMMIT;"; } | sqlite3 batch_onetxn.db
sqlite3 batch_autocommit.db < "$SEED"
# row-at-a-time, different statement order: insert customers in descending id order
python3 -I - "$SEED" > rowwise.sql <<'PY'
import sys
print("PRAGMA synchronous=OFF;")
print("CREATE TABLE customer(id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT UNIQUE, balance REAL);")
print("CREATE TABLE orders(id INTEGER PRIMARY KEY, customer_id INTEGER REFERENCES customer(id), note TEXT, amount INTEGER);")
print("CREATE INDEX orders_cust ON orders(customer_id);")
for i in range(1900,0,-1):
    bal = i*1.25*(2 if i%3==0 else 1)
    print(f"INSERT INTO customer VALUES({i},'name-{i}','user{i}@example.com',{bal!r});")
for i in range(1,6001):
    if i%5==0: continue
    note = 'x'*(50+(i%200)); note = note*2 if i%11==0 else note
    print(f"INSERT INTO orders VALUES({i},{(i*7)%2000+1},'{note}',{i*3});")
PY
sqlite3 batch_rowwise.db < rowwise.sql
for f in batch_onetxn batch_autocommit batch_rowwise; do printf '%-28s sha=%s size=%s\n' $f "$(sha256sum < $f.db | cut -c1-16)" "$(stat -c %s $f.db)"; done
echo "--- logical check (sqlite3 .sha3sum, content+schema hash):"
for f in batch_onetxn batch_autocommit batch_rowwise; do printf '%-28s %s\n' $f "$(sqlite3 $f.db .sha3sum | cut -c1-16)"; done
echo "--- header fields:"; python3 -I "$HERE/hdr.py" batch_onetxn.db batch_autocommit.db batch_rowwise.db delete.1.db wal.1.db autovac_full.1.db
echo "--- page diff onetxn vs autocommit:"; python3 -I "$HERE/pagediff.py" batch_onetxn.db batch_autocommit.db || true
echo "--- page diff onetxn vs autocommit, ignoring header bytes:"; python3 -I "$HERE/pagediff.py" batch_onetxn.db batch_autocommit.db --ignore-header || true
echo "--- page diff onetxn vs rowwise (ignoring header):"; python3 -I "$HERE/pagediff.py" batch_onetxn.db batch_rowwise.db --ignore-header | head -8 || true
echo "--- delete-mode vs wal-mode (after TRUNCATE checkpoint):"; python3 -I "$HERE/pagediff.py" delete.1.db wal.1.db || true
echo "--- delete vs spill_cache5 (ignoring header): cache spill changes only the stale freelist leaf:"; python3 -I "$HERE/pagediff.py" delete.1.db spill_cache5.1.db --ignore-header || true
