package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const packagePath = "ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process"

const rowsQuery = "SELECT count(*) || ' row(s): ' || coalesce(group_concat(who, ','), '') FROM (SELECT who FROM lab ORDER BY id)"

func prepare(ctx context.Context, lab *procs.Lab, dir, journal string) (string, *procs.Child, *procs.Child) {
	return prepareIn(ctx, lab, filepath.Join(dir, journal), procs.SQLOptions{JournalMode: journal})
}

func prepareIn(ctx context.Context, lab *procs.Lab, partDir string, options procs.SQLOptions) (string, *procs.Child, *procs.Child) {
	journal := options.JournalMode
	db := filepath.Join(partDir, "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, journal, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)"); err != nil {
		lab.Fatal("create", err)
	}
	options.Path = db
	holder := lab.SpawnSQL("holder", options)
	contender := lab.SpawnSQL("contender", options)
	fmt.Printf("INFO: holder pid %d, contender pid %d\n", holder.Pid, contender.Pid)
	return db, holder, contender
}

func kernelHolders(lab *procs.Lab, view procs.LockView, rangeName string, want map[int]string, claim string) {
	if !view.ProcListed {
		return
	}
	lab.Check(maps.Equal(view.ProcHolders(rangeName), want), claim)
}

func kernelNone(lab *procs.Lab, view procs.LockView, claim string) {
	if !view.ProcListed {
		return
	}
	lab.Check(len(view.Proc) == 0, claim)
}

func finalRows(ctx context.Context, lab *procs.Lab, db, journal string) procs.Inspection {
	lab.StopChildren()
	inspection, err := procs.InspectDatabase(ctx, db, journal, rowsQuery)
	if err != nil {
		lab.Fatal("inspect", err)
	}
	fmt.Printf("after both processes closed: integrity_check %s; %s\n", inspection.Integrity, inspection.Values[0])
	return inspection
}

func rollbackMode(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Println("== A. rollback mode (journal_mode=delete): the write lock is a byte-range lock on the database file")
	db, holder, contender := prepare(ctx, lab, dir, sqlitex.JournalDelete)
	ranges := procs.DatabaseLockRanges()
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	before := lab.Locks("database file", db, ranges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "RESERVED", holder.Pid) && procs.ReadLocked(before.Ranges, "SHARED") && procs.Unlocked(before.Ranges, "PENDING"),
		"the holder's open write transaction is a write lock on the RESERVED byte and a read lock on the SHARED range of the database file")
	kernelHolders(lab, before, "RESERVED", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: the RESERVED byte is write-locked by the holder alone")
	kernelHolders(lab, before, "SHARED", map[int]string{holder.Pid: "READ"}, "/proc/locks: the SHARED range is read-locked by the holder alone; the contender, with no transaction, holds nothing")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused the write lock: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "db")
	after := lab.Locks("database file", db, ranges)
	lab.Check(procs.Unlocked(after.Ranges), "after the holder opened and closed the database file with os.Open, none of its locks remain")
	kernelNone(lab, after, "/proc/locks: neither process holds any lock on the database file")
	inside := lab.Do(holder, "query", "SELECT count(*) FROM lab")
	lab.Check(inside.OK() && inside.Value == "1", "the holder still sees its uncommitted row: as far as SQLite knows, its write transaction is intact")
	admitted := lab.Do(contender, "begin", "immediate")
	lab.Check(admitted.OK(), "the second process is now admitted: BEGIN IMMEDIATE returns SQLITE_OK while the holder's transaction is still open")
	lab.Must(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')")
	during := lab.Locks("database file", db, ranges)
	lab.Check(procs.WriteLockedBy(during.Ranges, "RESERVED", contender.Pid), "the RESERVED byte is now write-locked by the second process")
	kernelHolders(lab, during, "SHARED", map[int]string{contender.Pid: "READ"}, "/proc/locks: the SHARED range is read-locked by the second process only; the holder, mid-transaction, holds no lock at all")
	contenderCommit := lab.Do(contender, "commit")
	lab.Check(contenderCommit.OK(), "the second process commits: its COMMIT returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.Code == sqlitex.CodeIOErrDeleteNoEnt,
		"the holder's COMMIT writes its pages, then fails to delete the rollback journal the other process already deleted: SQLITE_IOERR_DELETE_NOENT (5898)")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalDelete)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): holder",
		"the file passes integrity_check but holds only the holder's row: the commit that returned SQLITE_OK is lost")
}

func walMode(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Println("== B. WAL mode: the write lock is a byte-range lock on the -shm file")
	db, holder, contender := prepare(ctx, lab, dir, sqlitex.JournalWAL)
	shm := db + "-shm"
	shmRanges, dbRanges := procs.ShmLockRanges(), procs.DatabaseLockRanges()
	lab.Must(holder, "begin", "immediate")
	lab.Must(holder, "exec", "INSERT INTO lab VALUES (1, 'holder')")
	before := lab.Locks("-shm", shm, shmRanges)
	dbBefore := lab.Locks("database file", db, dbRanges)
	lab.Check(procs.WriteLockedBy(before.Ranges, "WAL_WRITE_LOCK", holder.Pid) && procs.ReadLocked(before.Ranges, "WAL_READ_LOCK(0)") && procs.ReadLocked(before.Ranges, "DMS"),
		"the holder's write transaction is a write lock on -shm byte 120 (WAL_WRITE_LOCK), next to its read lock on byte 123 (WAL_READ_LOCK(0))")
	lab.Check(procs.ReadLocked(dbBefore.Ranges, "SHARED") && procs.Unlocked(dbBefore.Ranges, "PENDING", "RESERVED"),
		"on the database file the SHARED range is read-locked and PENDING and RESERVED are free: WAL mode does not use RESERVED for writers")
	kernelHolders(lab, before, "WAL_WRITE_LOCK", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: -shm byte 120 is write-locked by the holder alone")
	kernelHolders(lab, before, "DMS", map[int]string{holder.Pid: "READ", contender.Pid: "READ"}, "/proc/locks: both processes read-lock the DMS byte 128 of -shm")
	kernelHolders(lab, dbBefore, "SHARED", map[int]string{holder.Pid: "READ", contender.Pid: "READ"}, "/proc/locks: both processes read-lock the SHARED range of the database file")
	refused := lab.Do(contender, "begin", "immediate")
	lab.Check(refused.Code == sqlitex.CodeBusy, "a second process is refused: BEGIN IMMEDIATE returns SQLITE_BUSY (5)")
	lab.Must(holder, "stray-close", "db")
	afterDB := lab.Locks("-shm", shm, shmRanges)
	dbAfter := lab.Locks("database file", db, dbRanges)
	lab.Check(procs.WriteLockedBy(afterDB.Ranges, "WAL_WRITE_LOCK", holder.Pid), "a stray close of the database file leaves the -shm write lock in place")
	kernelHolders(lab, dbAfter, "SHARED", map[int]string{contender.Pid: "READ"}, "/proc/locks: on the database file only the contender's SHARED read lock is left")
	kernelHolders(lab, afterDB, "WAL_WRITE_LOCK", map[int]string{holder.Pid: "WRITE"}, "/proc/locks: -shm byte 120 is still write-locked by the holder")
	stillRefused := lab.Do(contender, "begin", "immediate")
	lab.Check(stillRefused.Code == sqlitex.CodeBusy, "so the second process is still refused")
	lab.Must(holder, "stray-close", "-shm")
	afterShm := lab.Locks("-shm", shm, shmRanges)
	lab.Check(procs.Unlocked(afterShm.Ranges, "WAL_WRITE_LOCK", "WAL_READ_LOCK(0)"), "a stray close of the -shm file releases the holder's -shm locks")
	kernelHolders(lab, afterShm, "DMS", map[int]string{contender.Pid: "READ"}, "/proc/locks: the holder has no -shm lock left; only the contender's DMS read lock remains")
	if afterShm.ProcListed {
		lab.Check(len(afterShm.Proc) == 1, "/proc/locks: that DMS read lock is the only lock on -shm")
	}
	inside := lab.Do(holder, "query", "SELECT count(*) FROM lab")
	lab.Check(inside.OK() && inside.Value == "1", "the holder still sees its uncommitted row")
	admitted := lab.Do(contender, "begin", "immediate")
	lab.Check(admitted.OK(), "the second process is now admitted while the holder's transaction is still open")
	lab.Must(contender, "exec", "INSERT INTO lab VALUES (2, 'contender')")
	contenderCommit := lab.Do(contender, "commit")
	lab.Check(contenderCommit.OK(), "the second process commits: its COMMIT returns SQLITE_OK")
	holderCommit := lab.Do(holder, "commit")
	lab.Check(holderCommit.Code == sqlitex.CodeProtocol,
		"the holder's COMMIT finds the wal-index header changed under its write lock, retries, and gives up with SQLITE_PROTOCOL (15)")
	inspection := finalRows(ctx, lab, db, sqlitex.JournalWAL)
	lab.Check(inspection.Integrity == "ok" && inspection.Values[0] == "1 row(s): contender",
		"the file passes integrity_check and holds only the second process's row: the holder's transaction is lost, and the holder was told")
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.1")
	fmt.Printf("INFO: work dir %s, GOOS %s\n", sqlitex.DisplayPath(dir), runtime.GOOS)
	if !procs.ProcLocksSupported {
		trace.PrintLinuxOnly("the /proc/locks listing (the fcntl F_GETLK probe and every behavioural check still run)", packagePath)
	}
	lab := procs.NewLab(os.Stdout)
	rollbackMode(ctx, lab, dir)
	walMode(ctx, lab, dir)
	lab.Report("LOCKS BELONG TO THE PROCESS")
}
