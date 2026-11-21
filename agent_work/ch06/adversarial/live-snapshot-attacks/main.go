package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/capture"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/procs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

func clean(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	return strings.Join(strings.Fields(s), " ")
}

func report(id, tried, expect, got string, held bool) {
	status := "HELD"
	if !held {
		status = "BROKEN"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, clean(tried), clean(expect), clean(got), status)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func mustDo(script *procs.Script, c *procs.Child, action string, args ...string) procs.Result {
	r, err := script.Do(c, action, args...)
	if err != nil {
		fatal(c.Name+" "+action, err)
	}
	if !r.OK() {
		fatal(c.Name+" "+action, fmt.Errorf("unexpected %s (%s)", r.Outcome(), r.Error))
	}
	return r
}

func doRes(script *procs.Script, c *procs.Child, action string, args ...string) procs.Result {
	r, err := script.Do(c, action, args...)
	if err != nil {
		fatal(c.Name+" "+action, err)
	}
	return r
}

// buildReference builds the single-connection E1 reference matching 6.4's writer history.
func buildReference(ctx context.Context, r recipe.Recipe, path string, baseStep, targetStep int) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("ref mkdir", err)
	}
	if err := r.Build(ctx, path, baseStep); err != nil {
		fatal("ref build", err)
	}
	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("ref open", err)
	}
	for k := baseStep + 1; k <= targetStep; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			db.Close()
			fatal("ref apply", err)
		}
	}
	if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
		db.Close()
		fatal("ref ckpt", err)
	}
	if err := db.Close(); err != nil {
		fatal("ref close", err)
	}
	return path
}

// attackSnap01a reads pages 1..half in read tx 1 (step 8) and pages half+1..end in
// read tx 2 (after writer commits steps 9-10) into the same snapshot file.
func attackSnap01a(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap01a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap01a mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap01a build", err)
	}
	snapFile := filepath.Join(dir, "two-tx.db")

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("snap01a spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, PagesOut: snapFile}.Args()...)
	if err != nil {
		fatal("snap01a spawn c", err)
	}

	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}

	// Read transaction 1: capture pages 1..39 at step 8.
	mustDo(script, capturer, "begin", "deferred")
	mustDo(script, capturer, "user-version")
	pc1Str := mustDo(script, capturer, "query", "PRAGMA page_count").Value
	pc1, _ := strconv.Atoi(pc1Str)
	half := pc1 / 2
	mustDo(script, capturer, "dbpage-write", "1", strconv.Itoa(half))
	mustDo(script, capturer, "commit")

	// Writer advances to step 10 between the two read transactions.
	mustDo(script, writer, "recipe-step", "9")
	mustDo(script, writer, "recipe-step", "10")

	// Read transaction 2: capture remaining pages up to step 8's page count (79) from step 10!
	mustDo(script, capturer, "begin", "deferred")
	mustDo(script, capturer, "user-version")
	mustDo(script, capturer, "dbpage-write", strconv.Itoa(half+1), strconv.Itoa(pc1))
	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap01a close", err)
	}

	insp, err := procs.InspectDatabase(ctx, snapFile, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap01a inspect", err)
	}
	match, err := r.MatchStep(ctx, snapFile)
	if err != nil {
		fatal("snap01a match", err)
	}
	inconsistent := insp.OpenError != "" || insp.Integrity != "ok" || !match.Found()
	report(
		"SNAP-01a",
		"sqlite_dbpage capture split across TWO read transactions (step 8 pages 1-39 + step 10 pages 40-79)",
		"must be shown inconsistent: fails integrity_check or matches no recipe state",
		fmt.Sprintf("open_err=%q integrity=%s user_version=%s match=%s", insp.OpenError, procs.IntegritySummary(insp.Integrity), insp.Values[0], match.String()),
		inconsistent,
	)
}

// attackSnap01b tests a two-transaction sqlite_dbpage capture where page_count does NOT change:
// atomic 2-table transfer across half 1 and half 2 -> passes integrity_check=ok yet violates a+b==1000!
func attackSnap01b(ctx context.Context, root string) {
	dir := filepath.Join(root, "snap01b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap01b mkdir", err)
	}
	db := filepath.Join(dir, "ledger.db")
	stmts := []string{
		"CREATE TABLE ledger_a(id INTEGER PRIMARY KEY, bal INTEGER NOT NULL)",
		"INSERT INTO ledger_a VALUES (1, 500)",
		"CREATE TABLE spacer(id INTEGER PRIMARY KEY, pad TEXT NOT NULL)",
	}
	pad := strings.Repeat("S", 3500)
	for i := 1; i <= 30; i++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO spacer VALUES (%d, '%s')", i, pad))
	}
	stmts = append(stmts,
		"CREATE TABLE ledger_b(id INTEGER PRIMARY KEY, bal INTEGER NOT NULL)",
		"INSERT INTO ledger_b VALUES (1, 500)",
	)
	if err := procs.CreateDatabase(ctx, db, sqlitex.JournalWAL, stmts...); err != nil {
		fatal("snap01b create", err)
	}
	snapFile := filepath.Join(dir, "two-tx-constsize.db")

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("snap01b spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, PagesOut: snapFile}.Args()...)
	if err != nil {
		fatal("snap01b spawn c", err)
	}

	mustDo(script, capturer, "begin", "deferred")
	pc, _ := strconv.Atoi(mustDo(script, capturer, "query", "PRAGMA page_count").Value)
	half := pc / 2
	mustDo(script, capturer, "dbpage-write", "1", strconv.Itoa(half))
	mustDo(script, capturer, "commit")

	// Atomic transfer preserving page_count.
	mustDo(script, writer, "begin", "immediate")
	mustDo(script, writer, "exec", "UPDATE ledger_a SET bal = 400 WHERE id = 1")
	mustDo(script, writer, "exec", "UPDATE ledger_b SET bal = 600 WHERE id = 1")
	mustDo(script, writer, "commit")

	mustDo(script, capturer, "begin", "deferred")
	pc2, _ := strconv.Atoi(mustDo(script, capturer, "query", "PRAGMA page_count").Value)
	mustDo(script, capturer, "dbpage-write", strconv.Itoa(half+1), strconv.Itoa(pc2))
	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap01b close", err)
	}

	insp, err := procs.InspectDatabase(ctx, snapFile, sqlitex.JournalWAL,
		"SELECT (SELECT bal FROM ledger_a WHERE id=1) || '+' || (SELECT bal FROM ledger_b WHERE id=1) || '=' || ((SELECT bal FROM ledger_a WHERE id=1)+(SELECT bal FROM ledger_b WHERE id=1))")
	if err != nil {
		fatal("snap01b inspect", err)
	}
	held := pc == pc2 && insp.Integrity == "ok" && insp.Values[0] == "500+600=1100"
	report(
		"SNAP-01b",
		"sqlite_dbpage capture split across TWO read transactions on constant-size DB during atomic transfer",
		"even with identical page_count and integrity_check=ok, split capture violates atomicity (500+600=1100 != 1000)",
		fmt.Sprintf("page_count=%d->%d integrity=%s sum=%s", pc, pc2, insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackSnap02a runs Method 2 while a THIRD independent process (checkpointer) runs
// PASSIVE checkpoints mid-copy and writer commits before and after.
func attackSnap02a(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap02a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap02a mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap02a build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap02a spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap02a spawn cap", err)
	}
	ckpter, err := script.Spawn("checkpointer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap02a spawn ckpt", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("snap02a spawn cop", err)
	}

	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}
	// Capturer pins snapshot at step 8.
	mustDo(script, capturer, "begin", "deferred")
	mustDo(script, capturer, "user-version")

	half := mustDo(script, writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size").Value
	mustDo(script, copier, "copy-range", "db", "copy.db", "0", half)

	// Writer commits step 9, and the INDEPENDENT 3rd process checkpointer runs PASSIVE!
	mustDo(script, writer, "recipe-step", "9")
	ckptRes := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointPassive)

	mustDo(script, copier, "copy-range", "db", "copy.db", half, "end")
	mustDo(script, writer, "recipe-step", "10")
	mustDo(script, copier, "copy", "-wal", "copy.wal")

	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap02a close", err)
	}

	baseInsp, err := procs.InspectDatabase(ctx, filepath.Join(dir, "copy.db"), sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap02a base inspect", err)
	}
	baseMatch, err := r.MatchStep(ctx, filepath.Join(dir, "copy.db"))
	if err != nil {
		fatal("snap02a base match", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	if _, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed); err != nil {
		fatal("snap02a replay", err)
	}
	repInsp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap02a rep inspect", err)
	}
	repMatch, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("snap02a rep match", err)
	}

	baseTorn := baseInsp.Integrity != "ok" || !baseMatch.Found()
	replayOK := repInsp.Integrity == "ok" && repMatch.Found() && repMatch.Step == 10
	report(
		"SNAP-02a",
		"Method 2 file+WAL copy while a 3rd process runs checkpoint(PASSIVE) between halves and writer commits steps 9-10",
		"copy.db alone is torn, while walfile replay repairs it to recipe step 10 with integrity_check=ok",
		fmt.Sprintf("3rd_ckpt=(%s) base_torn=%v(int=%s) replay=(int=%s match=%s)", ckptRes.Value, baseTorn, procs.IntegritySummary(baseInsp.Integrity), repInsp.Integrity, repMatch.String()),
		baseTorn && replayOK,
	)
}

// attackSnap02b has a 3rd process attempt FULL, RESTART, and TRUNCATE checkpoints (timeout 0)
// mid-copy while capturer holds its read transaction.
func attackSnap02b(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap02b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap02b mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap02b build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap02b spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap02b spawn cap", err)
	}
	ckpter, err := script.Spawn("checkpointer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap02b spawn ckpt", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("snap02b spawn cop", err)
	}

	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}
	mustDo(script, capturer, "begin", "deferred")
	mustDo(script, capturer, "user-version")

	half := mustDo(script, writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size").Value
	mustDo(script, copier, "copy-range", "db", "copy.db", "0", half)

	mustDo(script, writer, "recipe-step", "9")
	resFull := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointFull)
	resRestart := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointRestart)
	resTrunc := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointTruncate)

	mustDo(script, copier, "copy-range", "db", "copy.db", half, "end")
	mustDo(script, writer, "recipe-step", "10")
	mustDo(script, copier, "copy", "-wal", "copy.wal")
	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap02b close", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	if _, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed); err != nil {
		fatal("snap02b replay", err)
	}
	repInsp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap02b inspect", err)
	}
	repMatch, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("snap02b match", err)
	}

	allBusy := strings.HasPrefix(resFull.Value, "busy=1") &&
		strings.HasPrefix(resRestart.Value, "busy=1") &&
		strings.HasPrefix(resTrunc.Value, "busy=1")
	replayOK := repInsp.Integrity == "ok" && repMatch.Found() && repMatch.Step == 10
	report(
		"SNAP-02b",
		"3rd process runs FULL/RESTART/TRUNCATE checkpoints mid-copy while capturer holds read txn",
		"all three stronger checkpoints report busy=1 (blocked by reader's mark) and file+WAL replay equals recipe step 10",
		fmt.Sprintf("FULL=(%s) RESTART=(%s) TRUNCATE=(%s) replay=(int=%s match=%s)", resFull.Value, resRestart.Value, resTrunc.Value, repInsp.Integrity, repMatch.String()),
		allBusy && replayOK,
	)
}

// attackSnap03a runs the repo's 6.4-live-snapshot experiment with -hold immediate
// using an isolated COURSE_WORK_DIR under _work/adv-ch06/.
func attackSnap03a(ctx context.Context, repoRoot, root string) {
	expWork := filepath.Join(root, "exp64-immediate-work")
	if err := os.MkdirAll(expWork, 0o755); err != nil {
		fatal("snap03a mkdir", err)
	}
	cmd := exec.CommandContext(ctx, "go", "run", "./ch06-locking-and-concurrency/experiments/6.4-live-snapshot", "-hold", "immediate")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+expWork)
	out, err := cmd.CombinedOutput()
	ok := err == nil && strings.Contains(string(out), "LIVE SNAPSHOT OK")
	summary := "exit=0 LIVE SNAPSHOT OK"
	if err != nil {
		summary = fmt.Sprintf("err=%v tail=%q", err, tailString(string(out), 200))
	}
	report(
		"SNAP-03a",
		"run 6.4-live-snapshot -hold immediate as subprocess",
		"exits 0 and prints LIVE SNAPSHOT OK",
		summary,
		ok,
	)
}

// attackSnap03b tests the "Production SQLite" BEGIN IMMEDIATE live-copy procedure directly
// on an UNCHECKPOINTED WAL (nBackfill=0) while a 3rd connection runs PASSIVE, RESTART,
// and TRUNCATE checkpoints between halves of the .db copy!
func attackSnap03b(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap03b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap03b mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap03b build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap03b spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap03b spawn cap", err)
	}
	ckpter, err := script.Spawn("checkpointer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap03b spawn ckpt", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("snap03b spawn cop", err)
	}

	// Commit steps 6..8 into WAL with nBackfill=0 (uncheckpointed).
	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}

	// Capturer holds BEGIN IMMEDIATE (Production SQLite procedure).
	mustDo(script, capturer, "begin", "immediate")
	// Verify writer is blocked from starting a write transaction.
	writerBlocked := doRes(script, writer, "begin", "immediate")

	// Copier copies first half of .db (still at step 5!).
	half := mustDo(script, writer, "query", "SELECT (page_count / 2) * page_size FROM pragma_page_count, pragma_page_size").Value
	mustDo(script, copier, "copy-range", "db", "copy.db", "0", half)

	// Another connection runs PASSIVE, RESTART, and TRUNCATE checkpoints right under BEGIN IMMEDIATE!
	ckptPassive := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointPassive)
	ckptRestart := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointRestart)
	ckptTrunc := mustDo(script, ckpter, "checkpoint", sqlitex.CheckpointTruncate)

	// Copier finishes second half of .db (now backfilled to step 8!) and copies -wal.
	mustDo(script, copier, "copy-range", "db", "copy.db", half, "end")
	mustDo(script, copier, "copy", "-wal", "copy.wal")
	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap03b close", err)
	}

	baseInsp, err := procs.InspectDatabase(ctx, filepath.Join(dir, "copy.db"), sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap03b base inspect", err)
	}
	baseMatch, err := r.MatchStep(ctx, filepath.Join(dir, "copy.db"))
	if err != nil {
		fatal("snap03b base match", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	if _, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed); err != nil {
		fatal("snap03b replay", err)
	}
	repInsp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap03b rep inspect", err)
	}
	repMatch, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("snap03b rep match", err)
	}

	passiveRan := strings.Contains(ckptPassive.Value, "checkpointed=61") && strings.HasPrefix(ckptPassive.Value, "busy=0")
	strongerBlocked := strings.HasPrefix(ckptRestart.Value, "busy=1") && strings.HasPrefix(ckptTrunc.Value, "busy=1")
	baseTorn := baseInsp.Integrity != "ok" || !baseMatch.Found()
	replayValid := repInsp.Integrity == "ok" && repMatch.Found() && repMatch.Step == 8

	held := writerBlocked.Code == sqlitex.CodeBusy && passiveRan && strongerBlocked && baseTorn && replayValid
	report(
		"SNAP-03b",
		"Production SQLite BEGIN IMMEDIATE on uncheckpointed WAL while 3rd connection runs PASSIVE/RESTART/TRUNCATE mid-copy",
		"writer blocked (BUSY 5); PASSIVE still runs and tears copy.db alone; RESTART/TRUNCATE busy=1; file+WAL replay repairs copy.db to step 8",
		fmt.Sprintf("writer=%s PASSIVE=(%s) RESTART=(%s) TRUNCATE=(%s) base_torn=%v(int=%s) replay=(int=%s match=%s)",
			writerBlocked.Outcome(), ckptPassive.Value, ckptRestart.Value, ckptTrunc.Value, baseTorn, procs.IntegritySummary(baseInsp.Integrity), repInsp.Integrity, repMatch.String()),
		held,
	)
}

// attackSnap04 ties back to 5.3: a slow capture reader pins the WAL while a writer
// with aggressive wal_autocheckpoint=20 commits 12 recipe steps (steps 7..18).
func attackSnap04(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap04 mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap04 build", err)
	}
	snapFile := filepath.Join(dir, "snapshot.db")

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{
		Path:              db,
		JournalMode:       sqlitex.JournalWAL,
		WALAutocheckpoint: 20,
	}.Args()...)
	if err != nil {
		fatal("snap04 spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, procs.SQLOptions{
		Path:        db,
		JournalMode: sqlitex.JournalWAL,
		PagesOut:    snapFile,
	}.Args()...)
	if err != nil {
		fatal("snap04 spawn c", err)
	}

	// Step 6 creates initial uncheckpointed WAL frames (< 20 or ~20).
	mustDo(script, writer, "recipe-step", "6")

	// Capturer pins snapshot at step 6.
	mustDo(script, capturer, "begin", "deferred")
	seen := mustDo(script, capturer, "user-version").Value
	pc, _ := strconv.Atoi(mustDo(script, capturer, "query", "PRAGMA page_count").Value)

	shmBefore, err := walfile.ParseIndexFile(db + "-shm")
	if err != nil {
		fatal("snap04 shm before", err)
	}
	saltBefore := shmBefore.Header().Salt1
	startMx := shmBefore.Header().MaxFrame

	// Read pages slowly in 4 chunks while writer commits steps 7..18 (3 steps per chunk)
	// with wal_autocheckpoint=20 enabled.
	chunkSize := (pc + 3) / 4
	prevWALSize := int64(0)
	monotonicGrowth := true
	stepK := 7
	for first := 1; first <= pc; first += chunkSize {
		last := min(first+chunkSize-1, pc)
		mustDo(script, capturer, "dbpage-write", strconv.Itoa(first), strconv.Itoa(last))
		for j := 0; j < 3 && stepK <= 18; j++ {
			mustDo(script, writer, "recipe-step", strconv.Itoa(stepK))
			stepK++
		}
		info, err := os.Stat(db + "-wal")
		if err != nil {
			fatal("snap04 stat wal", err)
		}
		if info.Size() <= prevWALSize {
			monotonicGrowth = false
		}
		prevWALSize = info.Size()
	}

	shmDuring, err := walfile.ParseIndexFile(db + "-shm")
	if err != nil {
		fatal("snap04 shm during", err)
	}
	pinnedBackfill := shmDuring.Checkpoint.Backfill
	duringMx := shmDuring.Header().MaxFrame

	// Release slow reader, checkpoint, and commit step 19 to trigger WAL restart (new salt-1).
	mustDo(script, capturer, "commit")
	ckptAfter := mustDo(script, writer, "checkpoint", sqlitex.CheckpointPassive)
	mustDo(script, writer, "recipe-step", "19")
	shmAfter, err := walfile.ParseIndexFile(db + "-shm")
	if err != nil {
		fatal("snap04 shm after", err)
	}
	saltAfter := shmAfter.Header().Salt1
	if err := script.Close(); err != nil {
		fatal("snap04 close", err)
	}

	// Verify slow reader's page capture is still E1-identical to recipe step 6!
	capDir := filepath.Join(dir, "cap")
	if _, err := capture.Pages(snapFile, capDir); err != nil {
		fatal("snap04 capture pages", err)
	}
	rebuilt := filepath.Join(dir, "rebuilt.db")
	if _, err := capture.Rebuild(capDir, rebuilt); err != nil {
		fatal("snap04 rebuild", err)
	}
	ref := buildReference(ctx, r, filepath.Join(dir, "ref", "r.db"), 5, 6)
	cmp, err := equal.CompareFiles(rebuilt, ref, 4096)
	if err != nil {
		fatal("snap04 compare", err)
	}

	// Check: WAL grew monotonically past autocheckpoint=20, nBackfill stayed pinned <= startMx,
	// capture matches step 6 at E1, and after reader ends checkpoint completes and salt-1 advances.
	held := seen == "6" &&
		monotonicGrowth &&
		duringMx > startMx &&
		duringMx > 100 &&
		pinnedBackfill <= startMx &&
		cmp.Equal &&
		saltAfter == saltBefore+1
	report(
		"SNAP-04",
		"slow sqlite_dbpage capture at step 6 while writer with wal_autocheckpoint=20 commits steps 7-18",
		"WAL grows monotonically past autocheckpoint=20, nBackfill stays pinned at reader mark, capture equals step 6 (E1), salt-1 increments after reader ends",
		fmt.Sprintf("monotonic=%v mxFrame=%d->%d pinned_nBackfill=%d e1_step6=%v ckpt_after=(%s) salt1=0x%08x->0x%08x",
			monotonicGrowth, startMx, duringMx, pinnedBackfill, cmp.Equal, ckptAfter.Value, saltBefore, saltAfter),
		held,
	)
}

// attackSnap05 demonstrates a subtle platform bug: holding BEGIN DEFERRED *without*
// issuing a first read statement does NOT acquire a WAL read mark, so checkpoint(RESTART)
// + a new commit overwrites the WAL under Method 2 and corrupts the replay.
func attackSnap05(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap05")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap05 mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap05 build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap05 spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap05 spawn cap", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("snap05 spawn cop", err)
	}

	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}

	// BUG: BEGIN DEFERRED with NO initial read acquires NO lock / NO read mark!
	mustDo(script, capturer, "begin", "deferred")
	mustDo(script, copier, "copy", "db", "copy.db")

	// Writer checkpoints RESTART (succeeds with busy=0!) and commits step 9 (restarts WAL!).
	ckpt := mustDo(script, writer, "checkpoint", sqlitex.CheckpointRestart)
	mustDo(script, writer, "recipe-step", "9")
	mustDo(script, copier, "copy", "-wal", "copy.wal")
	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap05 close", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	_, rerr := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed)
	if rerr != nil {
		report(
			"SNAP-05",
			"Method 2 holding BEGIN DEFERRED WITHOUT initial SELECT while writer runs checkpoint(RESTART) + step 9",
			"checkpoint(RESTART) succeeds (busy=0, no read mark held) and replay fails or yields corrupt/non-recipe DB",
			fmt.Sprintf("ckpt=(%s) replay_err=%v", ckpt.Value, rerr),
			strings.HasPrefix(ckpt.Value, "busy=0"),
		)
		return
	}
	insp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap05 inspect", err)
	}
	match, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("snap05 match", err)
	}
	held := strings.HasPrefix(ckpt.Value, "busy=0") && (insp.OpenError != "" || insp.Integrity != "ok" || !match.Found())
	report(
		"SNAP-05",
		"Method 2 holding BEGIN DEFERRED WITHOUT initial SELECT while writer runs checkpoint(RESTART) + step 9",
		"checkpoint(RESTART) succeeds (busy=0, no read mark held) and replay yields corrupt or non-recipe DB",
		fmt.Sprintf("ckpt=(%s) open_err=%q integrity=%s match=%s", ckpt.Value, insp.OpenError, procs.IntegritySummary(insp.Integrity), match.String()),
		held,
	)
}

// attackSnap06 tests Method 2 when the writer has spilled UNCOMMITTED frames into
// the tail of -wal (via small cache_size=5) and later rolls back.
func attackSnap06(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "snap06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("snap06 mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("snap06 build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}
	writer, err := script.Spawn("writer", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap06 spawn w", err)
	}
	capturer, err := script.Spawn("capture", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("snap06 spawn cap", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("snap06 spawn cop", err)
	}

	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}

	// Hold valid read transaction at step 8.
	mustDo(script, capturer, "begin", "deferred")
	mustDo(script, capturer, "user-version")

	// Shrink writer cache to 5 pages and start a large UNCOMMITTED write so frames spill to -wal.
	mustDo(script, writer, "exec", "PRAGMA cache_size=5")
	mustDo(script, writer, "begin", "immediate")
	bigBody := strings.Repeat("U", 8000)
	for i := 1; i <= 25; i++ {
		mustDo(script, writer, "exec", fmt.Sprintf("INSERT INTO documents(id, account_id, title, body) VALUES (%d, 1, 'uncommitted-%d', '%s')", 90000+i, i, bigBody))
	}

	// Copy .db and -wal WHILE those uncommitted spilled frames sit at the tail of -wal.
	mustDo(script, copier, "copy", "db", "copy.db")
	mustDo(script, copier, "copy", "-wal", "copy.wal")

	// Writer rolls back; capturer ends.
	mustDo(script, writer, "rollback")
	mustDo(script, capturer, "commit")
	if err := script.Close(); err != nil {
		fatal("snap06 close", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	rep, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed)
	if err != nil {
		fatal("snap06 replay", err)
	}
	insp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("snap06 inspect", err)
	}
	match, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("snap06 match", err)
	}

	held := rep.IgnoredFrameCount() > 0 && insp.Integrity == "ok" && match.Found() && match.Step == 8
	report(
		"SNAP-06",
		"Method 2 file+WAL copy while writer has spilled uncommitted tail frames (cache_size=5) then ROLLBACKs",
		"walfile replay ignores uncommitted tail frames (>0) and rebuilds cleanly to recipe step 8 (integrity_check=ok)",
		fmt.Sprintf("frames_in_wal=%d ignored=%d integrity=%s match=%s", rep.FramesInFile, rep.IgnoredFrameCount(), insp.Integrity, match.String()),
		held,
	)
}

func tailString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	workRoot := filepath.Join(repoRoot, "_work", "adv-ch06", "live-snapshot-attacks")
	if err := os.RemoveAll(workRoot); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}

	attackSnap01a(ctx, workRoot)
	attackSnap01b(ctx, workRoot)
	attackSnap02a(ctx, workRoot)
	attackSnap02b(ctx, workRoot)
	attackSnap03a(ctx, repoRoot, workRoot)
	attackSnap03b(ctx, workRoot)
	attackSnap04(ctx, workRoot)
	attackSnap05(ctx, workRoot)
	attackSnap06(ctx, workRoot)
}
