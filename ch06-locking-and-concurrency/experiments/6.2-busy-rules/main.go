package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	longTimeout   = 120 * time.Second
	stepDeadline  = 30 * time.Second
	countQuery    = "SELECT count(*) FROM lab"
	recipeBase    = 5
	recipeWrites  = 10
	insertPattern = "INSERT INTO lab(who) VALUES ('%s')"
)

func insert(who string) string { return fmt.Sprintf(insertPattern, who) }

func database(ctx context.Context, lab *procs.Lab, dir, name, journal string) string {
	db := filepath.Join(dir, name, "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, journal, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT NOT NULL)", insert("seed")); err != nil {
		lab.Fatal("create", err)
	}
	return db
}

func pair(ctx context.Context, lab *procs.Lab, dir, part, journal string, timeout time.Duration, first, second string) (*procs.Child, *procs.Child) {
	db := database(ctx, lab, dir, part, journal)
	options := procs.SQLOptions{Path: db, JournalMode: journal, BusyTimeout: timeout}
	fmt.Printf("journal_mode=%s, busy_timeout=%dms on both connections\n", journal, timeout.Milliseconds())
	return lab.SpawnSQL(first, options), lab.SpawnSQL(second, options)
}

func readersAndWriter(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Println("== A. WAL mode: a reader and a writer (busy_timeout 0, so any wait would show up as SQLITE_BUSY)")
	reader, writer := pair(ctx, lab, dir, "a-reader-writer", sqlitex.JournalWAL, 0, "reader", "writer")
	lab.Must(reader, "begin", "deferred")
	snapshot := lab.Must(reader, "query", countQuery)
	commits := []procs.Result{lab.Do(writer, "begin", "immediate"), lab.Do(writer, "exec", insert("writer")), lab.Do(writer, "commit")}
	lab.Check(allOK(commits), "with a read transaction open, the writer begins, writes and commits without waiting")
	again := lab.Must(reader, "query", countQuery)
	lab.Check(again.Value == snapshot.Value, "the reader still sees its snapshot ("+snapshot.Value+" rows) after the commit")
	lab.Must(reader, "commit")
	lab.Must(writer, "begin", "immediate")
	lab.Must(writer, "exec", insert("uncommitted"))
	lab.Must(reader, "begin", "deferred")
	during := lab.Do(reader, "query", countQuery)
	lab.Check(during.OK() && during.Value == "2", "with a write transaction open, a reader starts and reads the last committed state (2 rows) without waiting")
	lab.Must(writer, "commit")
	lab.Must(reader, "commit")
	lab.Must(reader, "begin", "deferred")
	lab.Must(reader, "query", countQuery)
	exclusive := []procs.Result{lab.Do(writer, "begin", "exclusive"), lab.Do(writer, "exec", insert("exclusive"))}
	readDuring := lab.Do(reader, "query", countQuery)
	exclusive = append(exclusive, lab.Do(writer, "commit"))
	lab.Check(allOK(exclusive) && readDuring.OK(), "BEGIN EXCLUSIVE in WAL mode behaves like BEGIN IMMEDIATE: it starts beside an open reader and the reader keeps reading")
	lab.Must(reader, "commit")
	lab.StopChildren()
}

func writersWithoutTimeout(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Println("== B. WAL mode: two writers, busy_timeout 0")
	first, second := pair(ctx, lab, dir, "b-writers-no-timeout", sqlitex.JournalWAL, 0, "writer-1", "writer-2")
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	deferredBegin := lab.Do(second, "begin", "deferred")
	deferredRead := lab.Do(second, "query", countQuery)
	deferredWrite := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(deferredBegin.OK() && deferredRead.OK() && deferredWrite.Code == sqlitex.CodeBusy,
		"BEGIN DEFERRED and a read succeed; the first write statement returns SQLITE_BUSY (5)")
	lab.Must(second, "rollback")
	immediate := lab.Do(second, "begin", "immediate")
	lab.Check(immediate.Code == sqlitex.CodeBusy, "BEGIN IMMEDIATE returns SQLITE_BUSY (5) at the BEGIN itself")
	lab.Must(first, "commit")
	lab.StopChildren()
}

func writersWithTimeout(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Printf("== C. WAL mode: two writers, busy_timeout %dms (each step must answer within %v, so a statement that waited out the timeout would fail the run)\n", longTimeout.Milliseconds(), stepDeadline)
	first, second := pair(ctx, lab, dir, "c-writers-timeout", sqlitex.JournalWAL, longTimeout, "writer-1", "writer-2")

	fmt.Println("-- C1. BEGIN IMMEDIATE waits for the lock")
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	waiting := lab.Begin(second, "begin", "immediate")
	blocked := lab.StillWaiting(waiting)
	lab.Must(first, "commit")
	admitted := lab.Finish(waiting)
	lab.Check(blocked && admitted.OK(), "BEGIN IMMEDIATE did not return while the other writer held the lock, and returned SQLITE_OK after it committed")
	lab.Must(second, "exec", insert("writer-2"))
	lab.Must(second, "commit")

	fmt.Println("-- C2. a deferred transaction with no read yet also waits at its first write")
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	lab.Must(second, "begin", "deferred")
	pending := lab.Begin(second, "exec", insert("writer-2"))
	blocked = lab.StillWaiting(pending)
	lab.Must(first, "commit")
	written := lab.Finish(pending)
	lab.Check(blocked && written.OK(), "the first statement of a deferred transaction waits like BEGIN IMMEDIATE and then succeeds")
	lab.Must(second, "commit")

	fmt.Println("-- C3. a deferred transaction that has already read cannot wait")
	lab.Must(second, "begin", "deferred")
	lab.Must(second, "query", countQuery)
	lab.Must(first, "begin", "immediate")
	lab.Must(first, "exec", insert("writer-1"))
	upgrade := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(upgrade.Code == sqlitex.CodeBusy, "its first write returns SQLITE_BUSY (5) at once although the busy timeout is 120000ms")
	lab.Must(first, "commit")
	stale := lab.Do(second, "exec", insert("writer-2"))
	lab.Check(stale.Code == sqlitex.CodeBusySnapshot, "after the other writer committed, the same write returns SQLITE_BUSY_SNAPSHOT (517) at once: the snapshot it read is no longer the newest")
	lab.Must(second, "rollback")
	retry := []procs.Result{lab.Do(second, "begin", "immediate"), lab.Do(second, "exec", insert("writer-2")), lab.Do(second, "commit")}
	lab.Check(allOK(retry), "the same write succeeds once the transaction is restarted")
	lab.StopChildren()
}

func rollbackContrast(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Println("== D. rollback mode (journal_mode=delete) as the contrast: a reader and a writer, busy_timeout 0")
	reader, writer := pair(ctx, lab, dir, "d-rollback", sqlitex.JournalDelete, 0, "reader", "writer")
	lab.Must(reader, "begin", "deferred")
	lab.Must(reader, "query", countQuery)
	writes := []procs.Result{lab.Do(writer, "begin", "immediate"), lab.Do(writer, "exec", insert("writer"))}
	lab.Check(allOK(writes), "the writer takes RESERVED and writes beside the reader's SHARED lock")
	blockedCommit := lab.Do(writer, "commit")
	lab.Check(blockedCommit.Code == sqlitex.CodeBusy, "the writer's COMMIT returns SQLITE_BUSY (5): it needs EXCLUSIVE and the reader holds SHARED")
	lab.Must(reader, "commit")
	newReader := lab.Do(reader, "query", countQuery)
	lab.Check(newReader.Code == sqlitex.CodeBusy, "a new read now returns SQLITE_BUSY (5): the writer kept PENDING after its failed COMMIT")
	retried := lab.Do(writer, "commit")
	lab.Check(retried.OK(), "the writer's second COMMIT succeeds once no reader is left")
	after := lab.Do(reader, "query", countQuery)
	lab.Check(after.OK() && after.Value == "2", "and the reader then reads the committed row")
	lab.StopChildren()
}

func recipeUnderContention(ctx context.Context, lab *procs.Lab, dir string) {
	fmt.Println()
	fmt.Printf("== E. two writers apply recipe steps %d-%d alternately with BEGIN IMMEDIATE and busy_timeout %dms; each step starts while the other writer holds the lock\n", recipeBase+1, recipeBase+recipeWrites, longTimeout.Milliseconds())
	r := recipe.Default()
	db := filepath.Join(dir, "e-recipe", "r.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		lab.Fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(db); err != nil {
		lab.Fatal("remove", err)
	}
	if err := r.Build(ctx, db, recipeBase); err != nil {
		lab.Fatal("build", err)
	}
	options := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: longTimeout}
	writers := []*procs.Child{lab.SpawnSQL("writer-1", options), lab.SpawnSQL("writer-2", options)}
	allWaited, allCommitted := true, true
	for k := recipeBase + 1; k < recipeBase+recipeWrites; k += 2 {
		holder, waiter := writers[0], writers[1]
		lab.Must(holder, "begin", "immediate")
		lab.Must(holder, "recipe-exec", strconv.Itoa(k))
		call := lab.Begin(waiter, "recipe-step", strconv.Itoa(k+1))
		allWaited = lab.StillWaiting(call) && allWaited
		allCommitted = lab.Do(holder, "commit").OK() && allCommitted
		allCommitted = lab.Finish(call).OK() && allCommitted
		writers[0], writers[1] = writers[1], writers[0]
	}
	last := lab.Must(writers[0], "user-version")
	lab.Check(allWaited && allCommitted, "every waiting BEGIN IMMEDIATE was still waiting while the other writer held the lock, and every transaction committed")
	lab.StopChildren()
	match, err := r.MatchStep(ctx, db)
	if err != nil {
		lab.Fatal("match", err)
	}
	fmt.Printf("user_version seen by the last writer: %s; the closed file equals recipe %s\n", last.Value, match)
	lab.Check(last.Value == strconv.Itoa(recipeBase+recipeWrites) && match.Found() && match.Step == recipeBase+recipeWrites,
		fmt.Sprintf("the final database equals the recipe's state after step %d", recipeBase+recipeWrites))
}

func allOK(results []procs.Result) bool {
	for _, r := range results {
		if !r.OK() {
			return false
		}
	}
	return true
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("6.2")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	lab := procs.NewLab(os.Stdout)
	lab.Script.Timeout = stepDeadline
	readersAndWriter(ctx, lab, dir)
	writersWithoutTimeout(ctx, lab, dir)
	writersWithTimeout(ctx, lab, dir)
	rollbackContrast(ctx, lab, dir)
	recipeUnderContention(ctx, lab, dir)
	lab.Report("BUSY RULES")
}
