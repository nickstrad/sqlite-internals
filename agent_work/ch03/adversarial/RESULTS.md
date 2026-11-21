# Chapter 3 — B-trees and page management — adversarial results (orchestrator's record)

Run 1 — 2026-11-21, on the Opus build, `run-all.sh`. Raw output: `results-run1.txt`.
16 HELD, 14 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| SPL-01 | random insert orders (3 fixed seeds x 250 rows of 1000-byte blobs) | root constant, integrity ok, changed<=WAL, and >50% of inserts change 1 page across all seeds | rootConst=true integrity=true changedInWAL=true [seed=11:1pg=86/250,splits=70,wal>changed=83,depth=2,pages=73; seed=42:1pg=78/250,splits=72,wal>changed=95,depth=2,pages=75; seed=99:1pg=86/250,splits=7 |
| SPL-03 | append 25 overflowing rows (5000B and 10000B > maxLocal 4061B) into a WAL table | headline claim 'most inserts change one page' holds (or breaks cleanly for overflowing payloads) | ovf1-5000B:1pg=0/25,min=3,max=5; ovf2-10000B:1pg=0/25,min=4,max=6 |
| SPL-05 | sequential ascending appends into a WITHOUT ROWID table (30 x 400B rows) | append split at depth 2 changes 3 pages (balance_quick) as 3.1 claims for append splits | rootConst=true depth2_append_split_changed_pages=[5 6] (WITHOUT ROWID uses index B-tree -> balance_nonroot, not balance_quick) |
| SPL-07b | mid-leaf DELETE 11..66 (56 rows, freeing leaves) with secure_delete=OFF; compare equal.ChangedPages against commit WAL frames | every page reported by equal.ChangedPages is in the commit's WAL frames | ChangedPages=17, byteChanged=5, walFrames=5, ChangedPages_not_in_WAL=12, byteChanged_not_in_WAL=0 |
| DEL-02 | DELETE 150 of 200 rows under auto_vacuum=NONE, FULL, and INCREMENTAL | unqualified claim 'file never shrinks on its own' holds across auto_vacuum modes (or breaks for FULL) | NONE:52->del:52(free=37)->vacuum:15; FULL:53->del:16(free=0); INC:53->del:53(free=37)->inc_vac:16 (noneOK=true incOK=true fullShrank=true) |
| DEL-05 | INSERT + DELETE 40 secret rows with secure_delete=ON (wal_autocheckpoint=0); inspect -wal after DELETE, after PASSIVE checkpoint, after TRUNCATE, and .db at res | deleted payload bytes are nowhere on disk once DELETE commits with secure_delete=ON | wal_after_delete=40/40, wal_after_PASSIVE=40/40, wal_after_TRUNCATE=0/40, db_at_rest=0/40 |
| DEL-06 | single transaction inserting 32 x 1000B rows (~8 pages) when freelist_count > 0 | 3.2 check wording 'page_count grows only in a transaction that started with an empty freelist' | freelist 2->0, page_count 12->18 (freelist exhausted mid-tx, then file grew by 6 pages in same tx) |
| EQ-03 | two DBs identical in schema order, cookie, rows, and all 6 LogicalPragmas, differing only in persistent PRAGMA default_cache_size (0 vs 2000) | if E3 says equal (same schema order & cookie), VACUUM INTO canonical forms match at E1 (and E2) | E3_equal=true, VACUUM_INTO_E1_equal=false(diffPages=1), VACUUM_INTO_E2_equal=false (bytes 48-51 copied by vacuum.c, not hashed by E3) |
| EQ-04 | LogicalHashFile on CREATE TABLE t(rowid INTEGER PRIMARY KEY, _rowid_ TEXT, oid TEXT) vs hidden-rowid table t(rowid TEXT, _rowid_ TEXT, oid TEXT) | IPK-rowid table hashes cleanly (rowid is NOT hidden); text-rowid table rejected with 'hide the rowid' | ipk_rowid_err=table "t": columns named rowid, _rowid_ and oid hide the rowid; hidden_rowid_err=table "t": columns named rowid, _rowid_ and oid hide the rowid |
| EQ-05 | run LogicalHashFile and RunDBHash on an uncheckpointed WAL database (.db + non-empty -wal) | read-only hashing leaves .db bytes and -wal file unchanged | wal_before=20632B; LogicalHashFile(db_changed=true,wal_removed=true); RunDBHash(db_changed=true,wal_removed=true) |
| EQ-08 | LogicalHash(ctx, conn) with TEMP TABLE t shadowing main.t + EncodeValue on unsupported types int(1) and bool(true) | LogicalHash hashes main.t identically regardless of TEMP table; EncodeValue does not silently collapse unsupported types to empty []byte | shadow_err=<nil>, shadow_matches_main=false, EncodeValue(int(1))= EncodeValue(true)= (empty_collision=true) |
| EQ-09 | cancel context mid-walk during LogicalHash(ctx, conn) and immediately issue BEGIN on the same conn | cleanup ends the read transaction even when ctx is canceled, so subsequent BEGIN succeeds | hitMidWalk=true, leakedTx=true (timeout=50us lhErr=read sqlite_schema: context deadline exceeded next_BEGIN=SQL logic error: cannot start a transaction within a transaction (1)) |
| R2-02 | R2 dump + replay + VACUUM INTO on DB with tables created in non-alphabetical order (zebra before alpha) | replayed DB is E3-equal to original AND its VACUUM INTO matches original's VACUUM INTO at E1 | E3_equal=true, VACUUM_INTO_E1_equal=false(diffPages=3), VACUUM_INTO_E2_equal=false (dump sorted tables alphabetically, changing sqlite_schema rowid order) |
| R2-03b | R2 dump + replay on (1) TEXT containing embedded NUL byte char(0), and (2) 1-col WITHOUT ROWID table "t?"("?" TEXT PRIMARY KEY) | both valid SQLite databases round-trip cleanly and match original at E3 | embedded_NUL:(ok=false replay_err=SQL logic error: unrecognized token: "'head" (1)); col_with_qmark:(ok=false replay_err=SQL logic error: no such table: t'val' (1)) |

Run 2 — after the Opus repair round (4bb8179): 22 HELD, 8 BROKEN (`results-run2.txt`).
The repair report (`../REPAIR-REPORT.md`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:

- SPL-01 — random insert orders (3 fixed seeds x 250 rows of 1000-byte blobs)
- SPL-03 — append 25 overflowing rows (5000B and 10000B > maxLocal 4061B) into a WAL table
- SPL-05 — sequential ascending appends into a WITHOUT ROWID table (30 x 400B rows)
- DEL-02 — DELETE 150 of 200 rows under auto_vacuum=NONE, FULL, and INCREMENTAL
- DEL-05 — INSERT + DELETE 40 secret rows with secure_delete=ON (wal_autocheckpoint=0); inspect -wal after DELETE, after PASSIVE checkpoint, after TRUNCATE, and 
- DEL-06 — single transaction inserting 32 x 1000B rows (~8 pages) when freelist_count > 0
- R2-02 — R2 dump + replay + VACUUM INTO on DB with tables created in non-alphabetical order (zebra before alpha)
- R2-03b — R2 dump + replay on (1) TEXT containing embedded NUL byte char(0), and (2) 1-col WITHOUT ROWID table "t?"("?" TEXT PRIMARY KEY)
