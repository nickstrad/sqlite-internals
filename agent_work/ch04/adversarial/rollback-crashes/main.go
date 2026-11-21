package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/walfleet/internal/journalfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const (
	dbName           = "t.db"
	markerBegin      = "marker-begin"
	markerEnd        = "marker-end"
	childStepMode    = "child-step"
	childSpillMode   = "child-spill"
	childOffLoopMode = "child-off-loop"
	childBirthMode   = "child-birth"
	childShrinkMode  = "child-shrink"
)

func init() {
	runtime.LockOSThread()
}

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func workDir() string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	dir := filepath.Join(root, "_work", "adv-ch04", "rollback-crashes")
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

func rollbackRecipe() recipe.Recipe {
	return recipe.Default().WithJournalMode(sqlitex.JournalDelete)
}

func stepOpts(journalMode, syncMode string, cacheSize int) sqlitex.Options {
	o := rollbackRecipe().SQLiteOptions()
	o.File.JournalMode = journalMode
	o.Conn.Synchronous = syncMode
	o.Conn.CacheSize = cacheSize
	return o
}

func touchMarker(dir, name string) {
	if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
		fatal("touch marker "+name, err)
	}
}

func writeMarkers(dir string) {
	for _, name := range []string{markerBegin, markerEnd} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			fatal("write marker", err)
		}
	}
}

func copyFile(from, to string) {
	data, err := os.ReadFile(from)
	if err != nil {
		fatal("copy read", err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		fatal("copy write", err)
	}
}

func prepareDir(dir, srcDB string) {
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean dir", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	if srcDB != "" {
		copyFile(srcDB, filepath.Join(dir, dbName))
	}
	writeMarkers(dir)
}

func canonicalHashFile(ctx context.Context, path string, opts sqlitex.Options) (string, string, error) {
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	var ic string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
		return "", "", err
	}
	target := path + ".canon"
	_ = os.Remove(target)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return ic, "", err
	}
	defer os.Remove(target)
	h, err := recipe.FileSHA256(target)
	if err != nil {
		return ic, "", err
	}
	return ic, h, nil
}

// --- Child roles ---

func runChildStep(dir, journalMode, syncMode string, stepNum int) {
	ctx := context.Background()
	step, err := rollbackRecipe().Step(stepNum)
	if err != nil {
		fatal("child step", err)
	}
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOpts(journalMode, syncMode, -2000))
	if err != nil {
		fatal("child open", err)
	}
	touchMarker(dir, markerBegin)
	if err := recipe.ApplyStep(ctx, db, step); err != nil {
		fatal("child apply", err)
	}
	touchMarker(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func runChildSpill(dir, syncMode string, cacheSize int) {
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOpts(sqlitex.JournalDelete, syncMode, cacheSize))
	if err != nil {
		fatal("spill open", err)
	}
	touchMarker(dir, markerBegin)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("spill begin", err)
	}
	payload := bytes.Repeat([]byte{0x7c}, 3500)
	if _, err := tx.ExecContext(ctx, "UPDATE t SET body = ?", payload); err != nil {
		fatal("spill update", err)
	}
	if err := tx.Commit(); err != nil {
		fatal("spill commit", err)
	}
	touchMarker(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("spill close", err)
	}
}

func runChildOffLoop(dir string, steps int) {
	ctx := context.Background()
	r := rollbackRecipe()
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOpts(sqlitex.JournalDelete, sqlitex.SyncOff, -2000))
	if err != nil {
		fatal("off-loop open", err)
	}
	defer db.Close()
	for k := 1; k <= steps; k++ {
		st, err := r.Step(k)
		if err != nil {
			fatal("off-loop step", err)
		}
		if err := recipe.ApplyStep(ctx, db, st); err != nil {
			fatal("off-loop apply", err)
		}
		fmt.Printf("ACK %d\n", k)
	}
}

func runChildBirth(dir string) {
	ctx := context.Background()
	touchMarker(dir, markerBegin)
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), sqlitex.DefaultOptions())
	if err != nil {
		fatal("birth open", err)
	}
	touchMarker(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("birth close", err)
	}
}

func runChildShrink(dir string) {
	ctx := context.Background()
	o := stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000)
	o.File.AutoVacuum = sqlitex.AutoVacuumFull
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), o)
	if err != nil {
		fatal("shrink open", err)
	}
	touchMarker(dir, markerBegin)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("shrink begin", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM t WHERE id > 10"); err != nil {
		fatal("shrink delete", err)
	}
	if err := tx.Commit(); err != nil {
		fatal("shrink commit", err)
	}
	touchMarker(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("shrink close", err)
	}
}

// --- Trace window extraction ---

type tracedOp struct {
	call    trace.Syscall
	ordinal int
	kind    string
}

func isSync(name string) bool   { return name == "fsync" || name == "fdatasync" }
func isOpen(name string) bool   { return name == "open" || name == "openat" }
func isUnlink(name string) bool { return name == "unlink" || name == "unlinkat" }
func isKillable(name string) bool {
	return name == "pwrite64" || name == "ftruncate" || isSync(name) || isUnlink(name)
}

func classifyOp(c trace.Syscall) string {
	j := dbName + "-journal"
	switch {
	case c.Name == "pwrite64" && c.Path == j && c.Offset == 0 && c.Length == 12:
		return "journal-count"
	case c.Name == "pwrite64" && c.Path == j && c.Offset == 0 && c.Length == 28:
		return "journal-zero-header"
	case c.Name == "pwrite64" && c.Path == j && c.Offset == 0:
		return "journal-header"
	case c.Name == "pwrite64" && c.Path == j:
		return "journal-record"
	case c.Name == "pwrite64" && c.Path == dbName:
		return "db-page"
	case c.Name == "pwrite64" && c.Path == dbName+"-wal":
		return "wal-write"
	case isSync(c.Name) && c.Path == j:
		return "journal-sync"
	case isSync(c.Name) && c.Path == ".":
		return "dir-sync"
	case isSync(c.Name) && c.Path == dbName:
		return "db-sync"
	case isSync(c.Name) && c.Path == dbName+"-wal":
		return "wal-sync"
	case isUnlink(c.Name) && c.Path == j:
		return "journal-unlink"
	case c.Name == "ftruncate" && c.Path == j:
		return "journal-truncate"
	case c.Name == "ftruncate" && c.Path == dbName:
		return "db-truncate"
	case isOpen(c.Name) && c.Path == markerEnd:
		return "step-returned"
	}
	return "other"
}

func extractOps(log *trace.StraceLog, dir string) ([]tracedOp, []tracedOp, error) {
	beginPath := filepath.Join(dir, markerBegin)
	endPath := filepath.Join(dir, markerEnd)
	tid := -1
	for _, c := range log.Calls {
		if isOpen(c.Name) && c.Path == beginPath {
			tid = c.PID
			break
		}
	}
	if tid < 0 {
		return nil, nil, errors.New("begin marker not found")
	}
	ordinals := map[string]int{}
	inside, ended := false, false
	var ops, targets []tracedOp
	for _, c := range log.Calls {
		if c.PID != tid || ended {
			continue
		}
		ordinals[c.Name]++
		if isOpen(c.Name) && c.Path == beginPath && !inside {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if isOpen(c.Name) && c.Path == endPath {
			rel := trace.RelativeTo(dir, []trace.Syscall{c})[0]
			targets = append(targets, tracedOp{call: rel, ordinal: ordinals[c.Name], kind: classifyOp(rel)})
			ended = true
			continue
		}
		if !trace.UnderDir(dir)(c) || strings.HasPrefix(filepath.Base(c.Path), "marker-") {
			continue
		}
		rel := trace.RelativeTo(dir, []trace.Syscall{c})[0]
		op := tracedOp{call: rel, ordinal: ordinals[c.Name], kind: classifyOp(rel)}
		ops = append(ops, op)
		if isKillable(c.Name) {
			targets = append(targets, op)
		}
	}
	if !ended {
		return nil, nil, errors.New("end marker not found")
	}
	return ops, targets, nil
}

func runTraced(ctx context.Context, args []string, kill *trace.KillAt) *trace.Result {
	cmd, err := trace.SelfCommand(args...)
	if err != nil {
		fatal("self command", err)
	}
	res, err := trace.Run(ctx, trace.Options{Command: cmd, KillAt: kill})
	if err != nil {
		fatal("trace.Run", err)
	}
	return res
}

// RCM-01 tests OUTLINE 4.1 bullets 1 & 2 across DELETE, TRUNCATE, and PERSIST:
// CheckAtRest refusal while hot, corruption when deleting/omitting -journal mid-write,
// silent commit when deleting -journal at db-sync, and clean recovery when copied together.
func attackRCM01(ctx context.Context, base, prePath, preCanon, postCanon string, postBytes []byte) {
	if !trace.Supported {
		report("RCM-01", "delete/omit hot -journal across DELETE/TRUNCATE/PERSIST", "strace required (Linux)", "skipped (!linux)", true)
		return
	}
	dir := filepath.Join(base, "rcm01")
	modes := []string{sqlitex.JournalDelete, sqlitex.JournalTruncate, sqlitex.JournalPersist}
	var issues []string

	for _, mode := range modes {
		bdir := filepath.Join(dir, mode+"-base")
		prepareDir(bdir, prePath)
		res := runTraced(ctx, []string{childStepMode, bdir, mode, sqlitex.SyncFull, "3"}, nil)
		if res.ExitCode != 0 {
			fatal("rcm01 baseline "+mode, fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
		}
		_, targets, err := extractOps(res.Log, bdir)
		if err != nil {
			fatal("rcm01 extract", err)
		}
		var dbWrites []tracedOp
		var dbSync tracedOp
		for _, t := range targets {
			if t.kind == "db-page" {
				dbWrites = append(dbWrites, t)
			} else if t.kind == "db-sync" {
				dbSync = t
			}
		}
		mid := dbWrites[len(dbWrites)/2]

		// 1. Mid-write kill
		mdir := filepath.Join(dir, mode+"-mid")
		prepareDir(mdir, prePath)
		kres := runTraced(ctx, []string{childStepMode, mdir, mode, sqlitex.SyncFull, "3"}, &trace.KillAt{Syscalls: []string{mid.call.Name}, When: mid.ordinal})
		if kres.Killed != "SIGKILL" {
			issues = append(issues, mode+" mid not killed")
			continue
		}
		midDB := filepath.Join(mdir, dbName)
		if err := recipe.CheckAtRest(midDB); err == nil {
			issues = append(issues, mode+" CheckAtRest accepted hot journal")
		}

		// Copy alone vs copy with journal
		aloneDir := filepath.Join(dir, mode+"-alone")
		bothDir := filepath.Join(dir, mode+"-both")
		_ = os.MkdirAll(aloneDir, 0o755)
		_ = os.MkdirAll(bothDir, 0o755)
		copyFile(midDB, filepath.Join(aloneDir, dbName))
		copyFile(midDB, filepath.Join(bothDir, dbName))
		copyFile(midDB+"-journal", filepath.Join(bothDir, dbName+"-journal"))

		// Delete journal in-place and reopen -> must NOT equal pre or post!
		if err := os.Remove(midDB + "-journal"); err != nil {
			fatal("remove mid journal", err)
		}
		icDel, canonDel, errDel := canonicalHashFile(ctx, midDB, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		if errDel == nil && icDel == "ok" && (canonDel == preCanon || canonDel == postCanon) {
			issues = append(issues, mode+" deleted-mid unexpectedly valid")
		}

		// Open alone copy -> must also be damaged
		icAlone, canonAlone, errAlone := canonicalHashFile(ctx, filepath.Join(aloneDir, dbName), stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		if errAlone == nil && icAlone == "ok" && (canonAlone == preCanon || canonAlone == postCanon) {
			issues = append(issues, mode+" copy-without-journal unexpectedly valid")
		}

		// Open both copy -> must recover to preCanon
		icBoth, canonBoth, errBoth := canonicalHashFile(ctx, filepath.Join(bothDir, dbName), stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		if errBoth != nil || icBoth != "ok" || canonBoth != preCanon {
			issues = append(issues, fmt.Sprintf("%s copy-with-journal failed (err=%v ic=%s)", mode, errBoth, icBoth))
		}

		// 2. Kill at db-sync, delete journal -> silently becomes post-step exact bytes!
		ldir := filepath.Join(dir, mode+"-late")
		prepareDir(ldir, prePath)
		lres := runTraced(ctx, []string{childStepMode, ldir, mode, sqlitex.SyncFull, "3"}, &trace.KillAt{Syscalls: []string{dbSync.call.Name}, When: dbSync.ordinal})
		if lres.Killed != "SIGKILL" {
			issues = append(issues, mode+" late not killed")
			continue
		}
		lateDB := filepath.Join(ldir, dbName)
		if err := os.Remove(lateDB + "-journal"); err != nil {
			fatal("remove late journal", err)
		}
		icLate, canonLate, errLate := canonicalHashFile(ctx, lateDB, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		lateBytes, _ := os.ReadFile(lateDB)
		if errLate != nil || icLate != "ok" || canonLate != postCanon || !bytes.Equal(lateBytes, postBytes) {
			issues = append(issues, fmt.Sprintf("%s late-delete did not silently commit (err=%v ic=%s)", mode, errLate, icLate))
		}
	}

	held := len(issues) == 0
	got := "all 3 modes (delete/truncate/persist): CheckAtRest refused hot journal; delete/omit mid-write caused corruption; copy-with-journal recovered pre-step; delete at db-sync silently committed post-step"
	if !held {
		got = strings.Join(issues, "; ")
	}
	report(
		"RCM-01",
		"delete/omit hot -journal mid-write and at db-sync across DELETE, TRUNCATE, PERSIST modes",
		"CheckAtRest refuses hot journal; losing journal corrupts mid-write or silently commits at db-sync; copying both recovers pre-step",
		got,
		held,
	)
}

func buildSpillDB(ctx context.Context, path string, rows int, autoVacuum string) {
	_ = recipe.RemoveDatabase(path)
	o := stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000)
	o.File.AutoVacuum = autoVacuum
	db, err := sqlitex.Open(ctx, path, o)
	if err != nil {
		fatal("build spill db", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create spill", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin spill", err)
	}
	rng := rand.New(rand.NewPCG(402, uint64(rows)))
	buf := make([]byte, 3500)
	for id := 1; id <= rows; id++ {
		for i := range buf {
			buf[i] = byte(rng.Uint32())
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, body) VALUES(?, ?)", id, buf); err != nil {
			fatal("insert spill", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit spill", err)
	}
	if err := db.Close(); err != nil {
		fatal("close spill", err)
	}
}

// RCM-02a tests synchronous=OFF under cache spill (cache_size=6 on 25-page UPDATE)
// with strace --inject kills across header, spill db-page, final db-page, unlink, and post.
func attackRCM02a(ctx context.Context, base string) {
	if !trace.Supported {
		report("RCM-02a", "synchronous=OFF spilling transaction killed at syscalls", "strace required (Linux)", "skipped (!linux)", true)
		return
	}
	dir := filepath.Join(base, "rcm02a")
	_ = os.MkdirAll(dir, 0o755)
	prePath := filepath.Join(dir, "pre.db")
	buildSpillDB(ctx, prePath, 25, sqlitex.AutoVacuumNone)
	preBytes, _ := os.ReadFile(prePath)

	bdir := filepath.Join(dir, "base")
	prepareDir(bdir, prePath)
	res := runTraced(ctx, []string{childSpillMode, bdir, sqlitex.SyncOff, "6"}, nil)
	if res.ExitCode != 0 {
		fatal("rcm02a base", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	postBytes, _ := os.ReadFile(filepath.Join(bdir, dbName))
	ops, targets, err := extractOps(res.Log, bdir)
	if err != nil {
		fatal("rcm02a extract", err)
	}
	syncCount := 0
	var dbPages []tracedOp
	var unlinkOp, endOp tracedOp
	for _, o := range ops {
		if isSync(o.call.Name) {
			syncCount++
		}
	}
	for _, t := range targets {
		switch t.kind {
		case "db-page":
			dbPages = append(dbPages, t)
		case "journal-unlink":
			unlinkOp = t
		case "step-returned":
			endOp = t
		}
	}
	sample := []tracedOp{targets[0], dbPages[0], dbPages[len(dbPages)/2], dbPages[len(dbPages)-1], unlinkOp, endOp}
	bad := 0
	preHits, postHits := 0, 0
	for i, t := range sample {
		kdir := filepath.Join(dir, fmt.Sprintf("k-%d", i))
		prepareDir(kdir, prePath)
		kres := runTraced(ctx, []string{childSpillMode, kdir, sqlitex.SyncOff, "6"}, &trace.KillAt{Syscalls: []string{t.call.Name}, When: t.ordinal})
		if kres.Killed != "SIGKILL" {
			bad++
			continue
		}
		kpath := filepath.Join(kdir, dbName)
		ic, _, err := canonicalHashFile(ctx, kpath, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		gotBytes, _ := os.ReadFile(kpath)
		switch {
		case err != nil || ic != "ok":
			bad++
		case bytes.Equal(gotBytes, preBytes) && t.kind != "step-returned":
			preHits++
		case bytes.Equal(gotBytes, postBytes) && t.kind == "step-returned":
			postHits++
		default:
			bad++
		}
	}
	held := syncCount == 0 && bad == 0 && preHits == len(sample)-1 && postHits == 1
	report(
		"RCM-02a",
		"synchronous=OFF spilling UPDATE (cache_size=6, 25 rows) killed across 6 syscall points",
		"0 syncs and every kill up to unlink recovers exact pre-tx bytes, post-unlink gives exact post-tx bytes",
		fmt.Sprintf("syncs=%d dbPages=%d preExact=%d postExact=%d failures=%d", syncCount, len(dbPages), preHits, postHits, bad),
		held,
	)
}

// RCM-02b stresses synchronous=OFF with 30 real asynchronous SIGKILLs across an
// 8-step rollback-mode recipe loop, verifying every kill recovers cleanly to a valid
// canonical recipe state k in {lastAck, lastAck+1}.
func attackRCM02b(ctx context.Context, base string) {
	dir := filepath.Join(base, "rcm02b")
	_ = os.MkdirAll(dir, 0o755)
	const maxSteps = 8
	const iterations = 30

	r := rollbackRecipe()
	canonByStep := make([]string, maxSteps+1)
	step0Path := filepath.Join(dir, "step0.db")
	for k := 0; k <= maxSteps; k++ {
		p := filepath.Join(dir, fmt.Sprintf("ref-%d.db", k))
		if err := r.Build(ctx, p, k); err != nil {
			fatal("build ref", err)
		}
		ic, h, err := canonicalHashFile(ctx, p, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		if err != nil || ic != "ok" {
			fatal("canon ref", fmt.Errorf("step %d ic=%s err=%v", k, ic, err))
		}
		canonByStep[k] = h
		if k == 0 {
			copyFile(p, step0Path)
		}
		_ = recipe.RemoveDatabase(p)
	}

	exe, err := os.Executable()
	if err != nil {
		fatal("exe", err)
	}
	rng := rand.New(rand.NewPCG(4022, 88))
	bad := 0
	killedMid := 0
	finishedAll := 0
	var firstErr string

	for i := range iterations {
		idir := filepath.Join(dir, fmt.Sprintf("iter-%02d", i))
		_ = os.MkdirAll(idir, 0o755)
		dbPath := filepath.Join(idir, dbName)
		copyFile(step0Path, dbPath)

		cmd := exec.Command(exe, childOffLoopMode, idir, strconv.Itoa(maxSteps))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fatal("stdout pipe", err)
		}
		if err := cmd.Start(); err != nil {
			fatal("start off-loop", err)
		}
		ackCh := make(chan int, maxSteps+1)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				var k int
				if _, err := fmt.Sscanf(sc.Text(), "ACK %d", &k); err == nil {
					ackCh <- k
				}
			}
			close(ackCh)
		}()

		delay := time.Duration(3+rng.IntN(45)) * time.Millisecond
		time.Sleep(delay)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		lastAck := 0
		for k := range ackCh {
			if k > lastAck {
				lastAck = k
			}
		}
		if lastAck == maxSteps {
			finishedAll++
		} else {
			killedMid++
		}

		ic, canon, err := canonicalHashFile(ctx, dbPath, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		matchedStep := -1
		for k, h := range canonByStep {
			if h == canon {
				matchedStep = k
				break
			}
		}
		if err != nil || ic != "ok" || matchedStep < lastAck || matchedStep > lastAck+1 {
			bad++
			if firstErr == "" {
				firstErr = fmt.Sprintf("iter=%d lastAck=%d matched=%d ic=%s err=%v", i, lastAck, matchedStep, ic, err)
			}
		}
		_ = os.RemoveAll(idir)
	}

	held := bad == 0 && killedMid > 0
	got := fmt.Sprintf("%d iterations (killedMid=%d finishedAll=%d) failures=%d", iterations, killedMid, finishedAll, bad)
	if firstErr != "" {
		got += " (" + firstErr + ")"
	}
	report(
		"RCM-02b",
		fmt.Sprintf("%d asynchronous SIGKILLs during 8-step synchronous=OFF rollback loop", iterations),
		"every kill reopens with integrity_check=ok to canonical recipe state k in [lastAck, lastAck+1]",
		got,
		held,
	)
}

// RCM-03a attacks 4.1's unqualified sync-count (4 FULL, 3 NORMAL) and strict
// all-journal-before-all-db ordering claims using a spilling transaction (cache_size=6).
// RCM-03b then kills that spilling FULL transaction at key points across segments
// to verify SQLite still preserves atomic all-or-nothing recovery.
func attackRCM03(ctx context.Context, base string) {
	if !trace.Supported {
		report("RCM-03a", "spilling commit sync count & ordering (FULL/NORMAL)", "strace required (Linux)", "skipped (!linux)", true)
		report("RCM-03b", "killing spilling FULL commit across multiple journal segments", "strace required (Linux)", "skipped (!linux)", true)
		return
	}
	dir := filepath.Join(base, "rcm03")
	_ = os.MkdirAll(dir, 0o755)
	prePath := filepath.Join(dir, "pre.db")
	buildSpillDB(ctx, prePath, 25, sqlitex.AutoVacuumNone)
	preBytes, _ := os.ReadFile(prePath)

	analyze := func(syncMode string) (syncs int, interleaved bool, targets []tracedOp, postBytes []byte) {
		bdir := filepath.Join(dir, "base-"+strings.ToLower(syncMode))
		prepareDir(bdir, prePath)
		res := runTraced(ctx, []string{childSpillMode, bdir, syncMode, "6"}, nil)
		if res.ExitCode != 0 {
			fatal("rcm03 base "+syncMode, fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
		}
		ops, tgts, err := extractOps(res.Log, bdir)
		if err != nil {
			fatal("rcm03 extract", err)
		}
		seenDBWrite := false
		for _, o := range ops {
			if isSync(o.call.Name) {
				syncs++
			}
			if o.kind == "db-page" {
				seenDBWrite = true
			}
			if seenDBWrite && (o.kind == "journal-record" || o.kind == "journal-header") {
				interleaved = true
			}
		}
		post, _ := os.ReadFile(filepath.Join(bdir, dbName))
		return syncs, interleaved, tgts, post
	}

	fullSyncs, fullInterleaved, fullTargets, postBytes := analyze(sqlitex.SyncFull)
	normSyncs, normInterleaved, _, _ := analyze(sqlitex.SyncNormal)

	// Narrowed claim (REPAIR-REPORT.md / build-notes.md:803-807): 4 FULL / 3 NORMAL
	// syncs and non-interleaved writes apply only to transactions that fit in cache;
	// spilling transactions emit extra sync cycles (>4 FULL, >3 NORMAL) and interleave
	// journal segments with database page writes.
	heldA := fullSyncs > 4 && normSyncs > 3 && fullSyncs > normSyncs && fullInterleaved && normInterleaved
	report(
		"RCM-03a",
		"25-row UPDATE with cache_size=6 (spilling via pagerStress) at synchronous=FULL and NORMAL",
		"per narrowed 4.1 claim: spilling commit exceeds 4 FULL / 3 NORMAL syncs (FULL > NORMAL) and interleaves journal segments with db-page writes",
		fmt.Sprintf("FULL: syncs=%d interleaved=%v; NORMAL: syncs=%d interleaved=%v", fullSyncs, fullInterleaved, normSyncs, normInterleaved),
		heldA,
	)

	// RCM-03b: Pick representative kill points spanning early spill, mid-segment-2,
	// final db-page, db-sync, journal-unlink, and step-returned.
	var jSyncs, dbWrites []tracedOp
	var dbSync, unlinkOp, endOp tracedOp
	for _, t := range fullTargets {
		switch t.kind {
		case "journal-sync":
			jSyncs = append(jSyncs, t)
		case "db-page":
			dbWrites = append(dbWrites, t)
		case "db-sync":
			dbSync = t
		case "journal-unlink":
			unlinkOp = t
		case "step-returned":
			endOp = t
		}
	}
	sample := []tracedOp{
		jSyncs[0],
		dbWrites[0],
		dbWrites[len(dbWrites)/3],
		dbWrites[2*len(dbWrites)/3],
		dbWrites[len(dbWrites)-1],
		dbSync,
		unlinkOp,
		endOp,
	}
	bad, preOK, postOK := 0, 0, 0
	for i, t := range sample {
		kdir := filepath.Join(dir, fmt.Sprintf("kill-%d", i))
		prepareDir(kdir, prePath)
		kres := runTraced(ctx, []string{childSpillMode, kdir, sqlitex.SyncFull, "6"}, &trace.KillAt{Syscalls: []string{t.call.Name}, When: t.ordinal})
		if kres.Killed != "SIGKILL" {
			bad++
			continue
		}
		kpath := filepath.Join(kdir, dbName)
		ic, _, err := canonicalHashFile(ctx, kpath, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		gotBytes, _ := os.ReadFile(kpath)
		switch {
		case err != nil || ic != "ok":
			bad++
		case bytes.Equal(gotBytes, preBytes) && t.kind != "step-returned":
			preOK++
		case bytes.Equal(gotBytes, postBytes) && t.kind == "step-returned":
			postOK++
		default:
			bad++
		}
	}
	heldB := bad == 0 && preOK == len(sample)-1 && postOK == 1
	report(
		"RCM-03b",
		fmt.Sprintf("kill spilling synchronous=FULL commit at %d points spanning multiple journal segments", len(sample)),
		"every kill up to journal-unlink recovers exact pre-tx bytes (integrity_check=ok); post-unlink gives post-tx bytes",
		fmt.Sprintf("preExact=%d/%d postExact=%d/1 failures=%d", preOK, len(sample)-1, postOK, bad),
		heldB,
	)
}

// RCM-04 tests build-notes Correction 4 (explicitly left unkilled in 4.1): killing
// sqlitex.Open(DefaultOptions()) on a brand-new file at EVERY write/sync/unlink of
// its WAL-birth commit, then retrying sqlitex.Open to verify crash-safe restart.
func attackRCM04(ctx context.Context, base string) {
	if !trace.Supported {
		report("RCM-04", "kill sqlitex.Open during brand-new WAL DB birth", "strace required (Linux)", "skipped (!linux)", true)
		return
	}
	dir := filepath.Join(base, "rcm04")
	bdir := filepath.Join(dir, "base")
	prepareDir(bdir, "")
	res := runTraced(ctx, []string{childBirthMode, bdir}, nil)
	if res.ExitCode != 0 {
		fatal("birth base", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	_, targets, err := extractOps(res.Log, bdir)
	if err != nil {
		fatal("birth extract", err)
	}

	leftHotJournal := 0
	leftColdJournal := 0
	retryFailures := 0
	var firstFail string

	for i, t := range targets {
		kdir := filepath.Join(dir, fmt.Sprintf("k-%02d", i))
		prepareDir(kdir, "")
		kres := runTraced(ctx, []string{childBirthMode, kdir}, &trace.KillAt{Syscalls: []string{t.call.Name}, When: t.ordinal})
		if kres.Killed != "SIGKILL" {
			retryFailures++
			if firstFail == "" {
				firstFail = fmt.Sprintf("k-%02d (%s) not killed", i, t.kind)
			}
			continue
		}
		kpath := filepath.Join(kdir, dbName)
		if j, err := journalfile.ReadFile(kpath + "-journal"); err == nil {
			if j.LooksHot() {
				leftHotJournal++
			} else {
				leftColdJournal++
			}
		}

		// Platform retry: call sqlitex.Open with DefaultOptions() on the crashed brand-new DB.
		db, err := sqlitex.Open(ctx, kpath, sqlitex.DefaultOptions())
		if err != nil {
			retryFailures++
			if firstFail == "" {
				info, _ := os.Stat(kpath)
				sz := int64(-1)
				if info != nil {
					sz = info.Size()
				}
				firstFail = fmt.Sprintf("k-%02d (%s %s) dbSize=%d retry Open err: %v", i, t.kind, t.call.Path, sz, err)
			}
			continue
		}
		var ic string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil || ic != "ok" {
			retryFailures++
			if firstFail == "" {
				firstFail = fmt.Sprintf("k-%02d ic=%s err=%v", i, ic, err)
			}
		}
		_ = db.Close()
	}

	held := retryFailures == 0 && leftHotJournal > 0
	got := fmt.Sprintf("%d kill points: leftHotJournal=%d leftColdJournal=%d retryFailures=%d",
		len(targets), leftHotJournal, leftColdJournal, retryFailures)
	if firstFail != "" {
		got += " (first: " + firstFail + ")"
	}
	report(
		"RCM-04",
		fmt.Sprintf("kill sqlitex.Open(DefaultOptions()) on a brand-new WAL DB at all %d write/sync/unlink points, then retry sqlitex.Open", len(targets)),
		"some kills leave a hot -journal beside the WAL-to-be DB, and retrying sqlitex.Open recovers cleanly at every kill point",
		got,
		held,
	)
}

// RCM-05 tests two rollback-journal boundaries:
// (a) hasHotJournal's 0-byte DB branch (pager.c:5224): a hot journal beside a 0-byte DB is deleted.
// (b) journal_mode=MEMORY and OFF killed mid-write: without an on-disk journal, a process kill corrupts the DB.
func attackRCM05(ctx context.Context, base, prePath, preCanon, postCanon string) {
	dir := filepath.Join(base, "rcm05")
	_ = os.MkdirAll(dir, 0o755)

	// Part A: 0-byte DB + hot journal -> SQLite deletes journal and opens clean 0-page DB.
	zeroDB := filepath.Join(dir, "zero.db")
	if err := os.WriteFile(zeroDB, nil, 0o644); err != nil {
		fatal("write zero db", err)
	}
	hdr := make([]byte, 512)
	copy(hdr[0:8], journalfile.Magic[:])
	// nRec=1, nonce=1, initialPages=1, sector=512, page=4096
	hdr[11] = 1
	hdr[15] = 1
	hdr[19] = 1
	hdr[22] = 2
	hdr[26] = 16
	if err := os.WriteFile(zeroDB+"-journal", hdr, 0o644); err != nil {
		fatal("write zero journal", err)
	}
	db, err := sql.Open("sqlite", zeroDB)
	if err != nil {
		fatal("open zero", err)
	}
	var pc int
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pc); err != nil {
		fatal("zero page_count", err)
	}
	db.Close()
	_, jErr := os.Stat(zeroDB + "-journal")
	zeroDeleted := pc == 0 && os.IsNotExist(jErr)

	// Part B: journal_mode=MEMORY and OFF killed mid-commit -> corrupt/neither pre nor post.
	if !trace.Supported {
		report("RCM-05", "0-byte DB hot-journal deletion + MEMORY/OFF mid-write kill", "zeroDeleted=true; MEMORY/OFF corrupt on kill", fmt.Sprintf("zeroDeleted=%v (strace skipped !linux)", zeroDeleted), zeroDeleted)
		return
	}
	var modeStatus []string
	allCorrupt := true
	for _, mode := range []string{sqlitex.JournalMemory, sqlitex.JournalOff} {
		bdir := filepath.Join(dir, mode+"-base")
		prepareDir(bdir, prePath)
		res := runTraced(ctx, []string{childStepMode, bdir, mode, sqlitex.SyncFull, "3"}, nil)
		if res.ExitCode != 0 {
			fatal("rcm05 "+mode, fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
		}
		_, targets, err := extractOps(res.Log, bdir)
		if err != nil {
			fatal("rcm05 extract", err)
		}
		var dbWrites []tracedOp
		for _, t := range targets {
			if t.kind == "db-page" {
				dbWrites = append(dbWrites, t)
			}
		}
		mid := dbWrites[len(dbWrites)/2]
		kdir := filepath.Join(dir, mode+"-kill")
		prepareDir(kdir, prePath)
		kres := runTraced(ctx, []string{childStepMode, kdir, mode, sqlitex.SyncFull, "3"}, &trace.KillAt{Syscalls: []string{mid.call.Name}, When: mid.ordinal})
		if kres.Killed != "SIGKILL" {
			allCorrupt = false
			modeStatus = append(modeStatus, mode+":not-killed")
			continue
		}
		ic, canon, err := canonicalHashFile(ctx, filepath.Join(kdir, dbName), stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
		damaged := err != nil || ic != "ok" || (canon != preCanon && canon != postCanon)
		if !damaged {
			allCorrupt = false
		}
		modeStatus = append(modeStatus, fmt.Sprintf("%s:damaged=%v(err=%v ic=%s)", mode, damaged, err != nil, ic))
	}

	held := zeroDeleted && allCorrupt
	report(
		"RCM-05",
		"0-byte DB with hot -journal (pager.c:5224) + mid-write kill under journal_mode=MEMORY and OFF",
		"0-byte DB deletes hot journal without error; MEMORY and OFF leave damaged/corrupt DB on mid-write SIGKILL",
		fmt.Sprintf("zeroDeleted=%v; %s", zeroDeleted, strings.Join(modeStatus, "; ")),
		held,
	)
}

// RCM-06 tests crash recovery on a SHRINKING transaction (auto_vacuum=FULL DELETE)
// killed on db-page, db-truncate (ftruncate on t.db), journal-unlink, and post-unlink.
func attackRCM06(ctx context.Context, base string) {
	if !trace.Supported {
		report("RCM-06", "shrinking auto_vacuum=FULL commit killed at db-page/ftruncate/unlink", "strace required (Linux)", "skipped (!linux)", true)
		return
	}
	dir := filepath.Join(base, "rcm06")
	_ = os.MkdirAll(dir, 0o755)
	prePath := filepath.Join(dir, "pre.db")
	buildSpillDB(ctx, prePath, 35, sqlitex.AutoVacuumFull)
	preInfo, _ := os.Stat(prePath)
	oAV := stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000)
	oAV.File.AutoVacuum = sqlitex.AutoVacuumFull
	_, preCanon, err := canonicalHashFile(ctx, prePath, oAV)
	if err != nil {
		fatal("rcm06 pre canon", err)
	}

	bdir := filepath.Join(dir, "base")
	prepareDir(bdir, prePath)
	res := runTraced(ctx, []string{childShrinkMode, bdir}, nil)
	if res.ExitCode != 0 {
		fatal("rcm06 base", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	postInfo, _ := os.Stat(filepath.Join(bdir, dbName))
	_, postCanon, err := canonicalHashFile(ctx, filepath.Join(bdir, dbName), oAV)
	if err != nil {
		fatal("rcm06 post canon", err)
	}
	_, targets, err := extractOps(res.Log, bdir)
	if err != nil {
		fatal("rcm06 extract", err)
	}

	var dbPages []tracedOp
	var dbTrunc, unlinkOp, endOp tracedOp
	for _, t := range targets {
		switch t.kind {
		case "db-page":
			dbPages = append(dbPages, t)
		case "db-truncate":
			dbTrunc = t
		case "journal-unlink":
			unlinkOp = t
		case "step-returned":
			endOp = t
		}
	}
	sample := []tracedOp{dbPages[len(dbPages)/2], unlinkOp, dbTrunc, endOp}
	unlinkIdx, truncIdx := -1, -1
	for i, t := range targets {
		if t.kind == "journal-unlink" {
			unlinkIdx = i
		}
		if t.kind == "db-truncate" {
			truncIdx = i
		}
	}
	var details []string
	allLogicalOK := true
	sawPreAtUnlink := false
	sawPostWithSlackAtTrunc := false
	sawExactPostAtEnd := false
	for i, t := range sample {
		if t.kind == "" {
			allLogicalOK = false
			details = append(details, fmt.Sprintf("#%d:missing", i))
			continue
		}
		kdir := filepath.Join(dir, fmt.Sprintf("k-%d", i))
		prepareDir(kdir, prePath)
		kres := runTraced(ctx, []string{childShrinkMode, kdir}, &trace.KillAt{Syscalls: []string{t.call.Name}, When: t.ordinal})
		if kres.Killed != "SIGKILL" {
			allLogicalOK = false
			details = append(details, t.kind+":not-killed")
			continue
		}
		kpath := filepath.Join(kdir, dbName)
		ic, canon, err := canonicalHashFile(ctx, kpath, oAV)
		info, _ := os.Stat(kpath)
		state := "other"
		switch {
		case err != nil || ic != "ok":
			state = fmt.Sprintf("err(%v/%s)", err, ic)
			allLogicalOK = false
		case canon == preCanon && info.Size() == preInfo.Size():
			state = fmt.Sprintf("pre(%dB)", info.Size())
			if t.kind == "journal-unlink" {
				sawPreAtUnlink = true
			}
		case canon == postCanon && info.Size() == postInfo.Size():
			state = fmt.Sprintf("post(%dB)", info.Size())
			if t.kind == "step-returned" {
				sawExactPostAtEnd = true
			}
		case canon == postCanon && info.Size() == preInfo.Size():
			// Committed (postCanon) with un-truncated trailing slack pages (pager.c:2155-2164).
			state = fmt.Sprintf("post-with-slack(%dB!=%dB)", info.Size(), postInfo.Size())
			if t.kind == "db-truncate" {
				sawPostWithSlackAtTrunc = true
			}
		default:
			allLogicalOK = false
		}
		details = append(details, fmt.Sprintf("%s->%s", t.kind, state))
	}
	// Corrected claim (REPAIR-REPORT.md / build-notes.md:808-817): pager_end_transaction
	// truncates a shrinking DB AFTER journal unlink (pager.c:2155-2164), so unlink precedes
	// db-truncate, killing on db-truncate leaves logically post-step content with trailing
	// slack pages, and logical atomicity ("never a third state") holds at every kill point.
	held := allLogicalOK && unlinkIdx >= 0 && truncIdx > unlinkIdx && sawPreAtUnlink && sawPostWithSlackAtTrunc && sawExactPostAtEnd
	report(
		"RCM-06",
		"shrinking auto_vacuum=FULL DELETE (no freelist) killed at db-page, journal-unlink, db-truncate, and step-returned",
		"per corrected build-notes (pager.c:2155-2164): unlink precedes db-truncate, kill at db-truncate leaves logical post with slack pages, logical atomicity holds everywhere",
		fmt.Sprintf("targetIdx(unlink=%d trunc=%d) size %d->%d: %s", unlinkIdx, truncIdx, preInfo.Size(), postInfo.Size(), strings.Join(details, ", ")),
		held,
	)
}

// RCM-07 is a "must be able to fail" attack (OUTLINE §1 item 4):
// (a) In a scratch repo copy with a mutated sqlite/PIN, both 4.1 and 4.2 MUST fail loudly (exit != 0).
// (b) Running 4.1 with SQLITE3_BIN pointing to a bogus CLI returning a wrong source id MUST exit != 0.
func attackRCM07(ctx context.Context, base string) {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(base, "scratch-repo")
	if err := os.MkdirAll(filepath.Join(scratch, "sqlite"), 0o755); err != nil {
		fatal("mkdir scratch", err)
	}
	for _, entry := range []string{"go.mod", "go.sum", "internal", "ch04-pager-and-page-cache"} {
		if err := os.Symlink(filepath.Join(root, entry), filepath.Join(scratch, entry)); err != nil {
			fatal("symlink "+entry, err)
		}
	}
	origPin, err := os.ReadFile(filepath.Join(root, "sqlite", "PIN"))
	if err != nil {
		fatal("read PIN", err)
	}
	mutatedPin := strings.ReplaceAll(string(origPin), "bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc", "0000000000000000000000000000000000000000000000000000000000000000")
	if err := os.WriteFile(filepath.Join(scratch, "sqlite", "PIN"), []byte(mutatedPin), 0o644); err != nil {
		fatal("write PIN", err)
	}

	runIn := func(dir string, extraEnv []string, args ...string) (int, time.Duration, string) {
		t0 := time.Now()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), extraEnv...)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		return code, time.Since(t0), string(out)
	}

	code41, _, _ := runIn(scratch, nil, "run", "./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal", "-exact-step", "0")
	code42, _, _ := runIn(scratch, nil, "run", "./ch04-pager-and-page-cache/experiments/4.2-cache-budget", "-n", "2", "-k", "1", "-rows", "20")

	// Also test 4.1 with a bogus SQLITE3_BIN wrapper returning wrong sqlite_source_id().
	fakeCLI := filepath.Join(base, "fake-sqlite3.sh")
	script := "#!/usr/bin/env bash\necho '2099-01-01 00:00:00 deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef'\n"
	if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
		fatal("write fake cli", err)
	}
	codeCLI, cliDur, cliOut := runIn(root, []string{"SQLITE3_BIN=" + fakeCLI}, "run", "./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal", "-exact-step", "0")
	cliDetected := codeCLI != 0 && strings.Contains(cliOut, "sqlite3 CLI: unavailable")

	held := code41 != 0 && code42 != 0 && cliDetected
	report(
		"RCM-07",
		"mutated sqlite/PIN on 4.1 and 4.2 (scratch repo) + bogus SQLITE3_BIN wrapper on 4.1",
		"4.1 exits non-zero, 4.2 exits non-zero, and 4.1 rejects bogus SQLITE3_BIN with non-zero exit",
		fmt.Sprintf("PIN-mismatch exits: 4.1=%d 4.2=%d; bogus SQLITE3_BIN on 4.1: exit=%d detected=%v duration=%s",
			code41, code42, codeCLI, cliDetected, cliDur.Round(100*time.Millisecond)),
		held,
	)
}

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case childStepMode:
			k, err := strconv.Atoi(os.Args[5])
			if err != nil {
				fatal("step atoi", err)
			}
			runChildStep(os.Args[2], os.Args[3], os.Args[4], k)
			return
		case childSpillMode:
			cs, err := strconv.Atoi(os.Args[4])
			if err != nil {
				fatal("cs atoi", err)
			}
			runChildSpill(os.Args[2], os.Args[3], cs)
			return
		case childOffLoopMode:
			n, err := strconv.Atoi(os.Args[3])
			if err != nil {
				fatal("n atoi", err)
			}
			runChildOffLoop(os.Args[2], n)
			return
		case childBirthMode:
			runChildBirth(os.Args[2])
			return
		case childShrinkMode:
			runChildShrink(os.Args[2])
			return
		}
	}

	ctx := context.Background()
	base := workDir()
	defer os.RemoveAll(base)

	// Build step 2 (pre) and step 3 (post) of the rollback recipe once.
	r := rollbackRecipe()
	prePath := filepath.Join(base, "step2.db")
	postPath := filepath.Join(base, "step3.db")
	if err := r.Build(ctx, prePath, 2); err != nil {
		fatal("build step2", err)
	}
	if err := r.Build(ctx, postPath, 3); err != nil {
		fatal("build step3", err)
	}
	postBytes, err := os.ReadFile(postPath)
	if err != nil {
		fatal("read post", err)
	}
	_, preCanon, err := canonicalHashFile(ctx, prePath, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
	if err != nil {
		fatal("pre canon", err)
	}
	_, postCanon, err := canonicalHashFile(ctx, postPath, stepOpts(sqlitex.JournalDelete, sqlitex.SyncFull, -2000))
	if err != nil {
		fatal("post canon", err)
	}

	attackRCM01(ctx, base, prePath, preCanon, postCanon, postBytes)
	attackRCM02a(ctx, base)
	attackRCM02b(ctx, base)
	attackRCM03(ctx, base)
	attackRCM04(ctx, base)
	attackRCM05(ctx, base, prePath, preCanon, postCanon)
	attackRCM06(ctx, base)
	attackRCM07(ctx, base)
}
