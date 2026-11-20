#!/usr/bin/env bash
# Spot-check source spans cited in OUTLINE.md against the pinned tree: the named symbol must
# appear within the first 12 lines of the cited span (function header) in the cited file.
# Usage: agent_work/outline-span-check.sh [pinned-tree]   exit 1 if any check fails
T=${1:-/root/Software/sqlite-src/3.53.4}; fail=0; n=0
chk(){ # file symbol start
  n=$((n+1))
  if sed -n "$3,$(( $3 + 12 ))p" "$T/$1" | grep -q "$2"; then :; else echo "MISS $1:$3 $2"; fail=1; fi; }
chk src/vdbeapi.c sqlite3_step 919;        chk src/vdbeapi.c sqlite3Step 777
chk src/prepare.c sqlite3LockAndPrepare 836; chk src/prepare.c sqlite3Prepare 682
chk src/tokenize.c sqlite3RunParser 600;   chk src/build.c sqlite3FinishCoding 141
chk src/vdbe.c OP_Transaction 4102;        chk src/vdbe.c OP_Insert 5748
chk src/vdbeaux.c sqlite3VdbeHalt 3315;    chk src/vdbeaux.c vdbeCommit 2919
chk src/os.c sqlite3OsRead 88
chk src/btree.c newDatabase 3510;          chk src/btree.c lockBtree 3282
chk src/btree.c decodeFlags 2028;          chk src/btree.c btreeInitPage 2218
chk src/btree.c btreeParseCellPtr 1259;    chk src/util.c sqlite3GetVarint 1633
chk src/vdbeaux.c sqlite3VdbeSerialGet 4123; chk src/vdbe.c OP_MakeRecord 3469
chk src/btree.c getOverflowPage 5007;      chk src/btree.c accessPayload 5125
chk src/btree.c allocateBtreePage 6514;    chk src/btree.c freePage2 6836
chk src/pager.c pager_write_changecounter 3130
chk src/btree.c sqlite3BtreeInsert 9409;   chk src/btree.c fillInCell 7074
chk src/btree.c insertCellFast 7428;       chk src/btree.c allocateSpace 1819
chk src/btree.c pageFindSlot 1747;         chk src/btree.c defragmentPage 1613
chk src/btree.c "balance(" 9130;           chk src/btree.c balance_quick 8007
chk src/btree.c balance_deeper 9049;       chk src/btree.c balance_nonroot 8245
chk src/btree.c editPage 7873;             chk src/btree.c rebuildPage 7644
chk src/btree.c checkTreePage 10855
chk src/pager.c sqlite3PagerSharedLock 5300; chk src/pager.c hasHotJournal 5180
chk src/pager.c sqlite3PagerCommitPhaseOne 6511; chk src/pager.c pager_end_transaction 2052
chk src/pager.c pager_playback 2864;       chk src/pager.c pagerStress 4655
chk src/wal.c walChecksumBytes 856;        chk src/wal.c walFrames 4038
chk src/pager.c pagerWalFrames 3225;       chk src/wal.c walTryBeginRead 3020
chk src/wal.c walCheckpoint 2199;          chk src/wal.c sqlite3WalCheckpoint 4301
chk src/wal.c walRestartHdr 2152;          chk src/wal.c walRestartLog 3875
chk src/wal.c walIndexRecover 1390;        chk src/main.c sqlite3WalDefaultHook 2484
chk src/os_unix.c unixLock 1856;            chk src/os_unix.c posixUnlock 2124
chk src/os_unix.c unixShmLock 5284;        chk src/pager.c sqlite3PagerBegin 5968
chk src/main.c sqliteDefaultBusyCallback 1730
chk src/os_unix.c unixOpen 6538;           chk src/os_unix.c robust_open 834
chk src/os_unix.c full_fsync 3778;         chk src/os_unix.c unixDelete 6833
chk src/main.c openDatabase 3360;          chk src/btree.c sqlite3BtreeOpen 2532
chk src/prepare.c sqlite3InitOne 199;      chk src/prepare.c sqlite3Init 438
chk src/attach.c attachFunc 74
chk src/pager.c pager_delsuper 2579;       chk src/backup.c sqlite3_backup_step 323
chk src/vacuum.c sqlite3RunVacuum 143;     chk src/memdb.c sqlite3_serialize 750
chk src/btree.c sqlite3BtreeIntegrityCheck 11144
for f in ext/misc/vfstrace.c src/test_demovfs.c ext/misc/vfsstat.c src/test6.c tool/dbhash.c tool/showwal.c tool/showjournal.c tool/sqlite3_rsync.c tool/sqldiff.c doc/pager-invariants.txt src/dbpage.c src/dbstat.c src/vdbevtab.c src/btree.h src/pager.h src/pcache.h src/wal.h src/parse.y src/status.c ext/misc/cksumvfs.c; do
  n=$((n+1)); [ -f "$T/$f" ] || { echo "MISSING FILE $f"; fail=1; }; done
echo "checked $n, fail=$fail"; exit $fail
