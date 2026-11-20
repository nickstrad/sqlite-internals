# Chapter 2 — adversarial results (orchestrator's record)

Run 1 — 2026-11-20, on the Opus build with Neon's comments, `run-all.sh`. Raw output: `results-run1.txt`.
22 HELD, 14 BROKEN. Every BROKEN line went to the repair round to be fixed or answered:

| Attack | What was tried | Expected | Got |
| --- | --- | --- | --- |
| HDR-03 | WAL mode on ONE connection: 3 page-1 commits separated by PRAGMA shrink_memory or cache_size=2 scan | 2.1 C1 one-connection rule predicts constant counter [9 9 9] | base=8, shrink_memory=[9 10 11] (final=11), cache_size=2=[9 9 9] (final=9) |
| HDR-06 | negative user_version/application_id/default_cache_size (-1,-1,-100) + all 3 encodings x 3 auto_vacuum modes | dbfile.Header agrees with PRAGMA and CLI .dbinfo on every field | diffs=2 ([user_version: dbfile=-1 dbinfo=4294967295 application_id: dbfile=-1 dbinfo=4294967295]) |
| HDR-08 | header validation on 9 malformed headers + mismatched write_version=1/read_version=2 vs SQLite | all 9 malformed headers rejected with ErrNotADatabase, and WALMode() agrees with SQLite PRAGMA journal_mode | malformed_accepted=[], w=1/r=2: dbfile_err=<nil> WALMode=false sqlite_journal_mode="wal"(err=<nil>) |
| HDR-10 | run experiment 2.1 with SQLITE3_BIN pointing to a wrong-version sqlite3 CLI | 2.1 exits non-zero when an explicit SQLITE3_BIN fails the pin check (OUTLINE §1 item 4) | exit=0, line2="NO-CLI: sqlite3 CLI _work/adv-ch02/header-falsify/fake-sqlite3.sh reports sqlite_source_id \"2099-01-01 00:00:00 deadbeefdeadbeefdeadbeefdeadbe |
| CEN-03 | census a valid SQLite database containing a WITHOUT ROWID table (with overflow and secondary index) | dbfile.Census succeeds, Complete()==nil, and matches dbstat (SQLite integrity_check=ok) | sqlite_ic="ok", census_err=tree "wr" (root 2): database file is malformed: page 2: root of table "wr" is a index-leaf page, diffs=[] |
| COR-02 | corrupt page-2 header fields (fragmented=200, zero real freeblock, lower content start) vs SQLite integrity_check | inspector reports an error on every page-header corruption that SQLite integrity_check rejects | missed=3 ([byte7-fragmented=200(origFB=4066,dbfile_err=<nil>,sqlite="*** in database main *** / Tree 2 page 2: free space corruption") byte1-2-zero-existing-freeblock(origFB=4066,dbfile_err=<nil>,sqli |
| COR-03b | corrupt LAST overflow page's next pointer (to self and to 999999) vs SQLite integrity_check | dbfile rejects non-zero next pointer on last overflow page just as SQLite integrity_check does | missed=2 ([lastOvfl-next=self(dbfile_err=<nil>,sqlite="*** in database main *** / Tree 2 page 2 cell 0: 2nd reference to page 4") lastOvfl-next=999999(dbfile_err=<nil>,sqlite="*** in database main *** |
| COR-05b | duplicate cell pointer on page 2 (two cells overlapping the exact same byte range) vs SQLite integrity_check | inspector reports an error on overlapping cells just as SQLite integrity_check does | dbfile_err=<nil>, panic=<nil>, sqlite_ic="*** in database main *** / Tree 2 page 2 cell 0: Rowid 1 out of order / (+2)" |
| CAP-02a | drop last page from BOTH manifest.txt (updating pages N-1) and pages.bin, then call capture.Rebuild | Rebuild rejects the capture because page 1 header declares N pages while capture has N-1 | rebuild_err=<nil>, sqlite_on_rebuilt="err: database disk image is malformed (11)" |
| CAP-02b | replace page 1's role in manifest.txt with 'bogus-role' and with 'overflow', keeping sha256 valid | Rebuild rejects unknown or mismatched page roles in the manifest | accepted_tampered_roles=[bogus-role overflow] |
| CAP-02c | feed a 0-page manifest (pages 0 + empty pages.bin) to capture.Rebuild | Rebuild rejects a 0-page manifest rather than writing an empty 0-byte file | rebuild_err=<nil>, out_size=0 |
| CAP-04 | capture a WAL DB while a live connection holds an open write tx after wal_checkpoint(TRUNCATE) (-wal=0 bytes, -shm=32768 bytes) | capture refuses an unclosed database whose -wal and -shm side files are live (OUTLINE 2.3) | -wal=0 bytes, -shm=32768 bytes, CheckAtRest=<nil>, capture.Pages_err=<nil> |
| CAP-05 | trigger mid-capture WAL write during capture.Pages (hit on attempt 1) | failed capture.Pages removes partial/stale pages.bin and manifest.txt from target dir | err="database changed during the capture: database is not at rest: _work/adv-ch02/manifest-capture/race-1.db: -wal holds 32 bytes of frames not yet checkpoint |
| CAP-07 | capture.Pages on a closed, valid SQLite database containing a WITHOUT ROWID table | R1 capture succeeds on any closed valid SQLite database (integrity_check=ok) | sqlite_ic="ok", capture.Pages_err=tree "wr" (root 2): database file is malformed: page 2: root of table "wr" is a index-leaf page |

Run 2 — after the Opus repair round (7690293): 33 HELD, 3 BROKEN (`results-run2.txt`).
The repair report (`../REPAIR-REPORT.md`) says for each run-1 finding whether it was fixed or answered. Still BROKEN in run 2:

- HDR-03 — WAL mode on ONE connection: 3 page-1 commits separated by PRAGMA shrink_memory or cache_size=2 scan
- HDR-06 — negative user_version/application_id/default_cache_size (-1,-1,-100) + all 3 encodings x 3 auto_vacuum modes
- CAP-04 — capture a WAL DB while a live connection holds an open write tx after wal_checkpoint(TRUNCATE) (-wal=0 bytes, -shm=32768 bytes)
