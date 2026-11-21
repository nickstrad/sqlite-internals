package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
	"github.com/nickstrad/walfleet/internal/walfile"
	"github.com/nickstrad/walfleet/internal/walship"
)

const (
	modeShipOpen      = "child-ship-open"
	modeShipCycle     = "child-ship-cycle"
	modeSpillAndCrash = "child-spill-crash"
)

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

func copyDir(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		return copyFile(path, dst)
	})
}

func applySessionStep(ctx context.Context, r recipe.Recipe, dbPath string, k int) error {
	opts := r.SQLiteOptions()
	opts.SingleConnection = true
	opts.ImmediateTx = true
	opts.Conn.WALAutocheckpoint = 0
	opts.Conn.BusyTimeout = 5 * time.Second
	db, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return err
	}
	applyErr := r.ApplyStep(ctx, db, k)
	return errors.Join(applyErr, db.Close())
}

// runShipOpenChild opens walship on -db and -store (taking the initial snapshot) and exits.
func runShipOpenChild(args []string) error {
	fs := flag.NewFlagSet(modeShipOpen, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	storeDir := fs.String("store", "", "")
	every := fs.Int("every", 2, "")
	origin := fs.Int64("origin", 2, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	store, err := walship.NewDirStore(*storeDir)
	if err != nil {
		return err
	}
	s, err := walship.Open(ctx, walship.Config{
		Database:      *dbPath,
		Store:         store,
		Options:       recipe.Default().SQLiteOptions(),
		SnapshotEvery: *every,
		Origin:        *origin,
	})
	if err != nil {
		return err
	}
	return s.Close()
}

// runShipCycleChild reopens walship on -db and -store, runs one Cycle(), and exits.
func runShipCycleChild(args []string) error {
	fs := flag.NewFlagSet(modeShipCycle, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	storeDir := fs.String("store", "", "")
	every := fs.Int("every", 2, "")
	origin := fs.Int64("origin", 2, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	store, err := walship.NewDirStore(*storeDir)
	if err != nil {
		return err
	}
	s, err := walship.Open(ctx, walship.Config{
		Database:      *dbPath,
		Store:         store,
		Options:       recipe.Default().SQLiteOptions(),
		SnapshotEvery: *every,
		Origin:        *origin,
	})
	if err != nil {
		return err
	}
	_, cycErr := s.Cycle(ctx)
	closeErr := s.Close()
	return errors.Join(cycErr, closeErr)
}

// runSpillCrashChild opens a database with cache_size=2, begins a write transaction,
// inserts enough large rows to spill uncommitted frames into -wal, and exits immediately
// via os.Exit(0) without committing or closing.
func runSpillCrashChild(args []string) error {
	fs := flag.NewFlagSet(modeSpillAndCrash, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	r := recipe.Default()
	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0
	opts.Conn.CacheSize = 2
	opts.Conn.BusyTimeout = 5 * time.Second
	db, err := sqlitex.Open(ctx, *dbPath, opts)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	payload := strings.Repeat("Z", 3500)
	for i := 1; i <= 30; i++ {
		if _, err := conn.ExecContext(ctx, "INSERT INTO events(id, account_id, kind, amount, note) VALUES (?, 1, 'crash', 1, ?)", 900000+i, payload); err != nil {
			return err
		}
	}
	// Exit without committing or closing, leaving spilled uncommitted frames in -wal.
	os.Exit(0)
	return nil
}

func mainThreadOf(res *trace.Result, storeRoot string) int {
	counts := map[int]int{}
	for _, c := range res.Log.Calls {
		if strings.HasPrefix(c.Path, storeRoot) || c.Name == "pwrite64" || c.Name == "write" {
			counts[c.PID]++
		}
	}
	best, most := 0, 0
	for pid, n := range counts {
		if n > most {
			best, most = pid, n
		}
	}
	return best
}

// verifyStoreRestoresAll loads catalog, checks 0 damaged objects, and verifies every
// position [minPos..last] restores byte-identical to its recipe step (via UserVersion).
func verifyStoreRestoresAll(ctx context.Context, store walship.Store, hashes []recipe.FileHashes, minPos int64, wantLastStep int) error {
	keys, err := store.List(ctx, "")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if strings.Contains(k, ".tmp") {
			return fmt.Errorf("store.List leaked temp key %q", k)
		}
	}
	cat, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		return err
	}
	if len(cat.Damaged) > 0 {
		return fmt.Errorf("catalog has %d damaged object(s): %+v", len(cat.Damaged), cat.Damaged[0])
	}
	last, ok := cat.LastPosition()
	if !ok || last < minPos {
		return fmt.Errorf("last position %d (ok=%t), want >= %d", last, ok, minPos)
	}
	var lastUV int
	for pos := minPos; pos <= last; pos++ {
		plan, err := cat.Plan(pos)
		if err != nil {
			return fmt.Errorf("plan %d: %w", pos, err)
		}
		img, _, err := plan.Execute()
		if err != nil {
			return fmt.Errorf("exec %d: %w", pos, err)
		}
		uv := int(plan.Commit.UserVersion)
		if uv < 0 || uv >= len(hashes) {
			return fmt.Errorf("pos %d: unexpected user_version %d", pos, uv)
		}
		if walship.SHA256Hex(img) != hashes[uv].Exact {
			return fmt.Errorf("pos %d (step %d): restored sha256 differs from recipe", pos, uv)
		}
		lastUV = uv
	}
	if lastUV != wantLastStep {
		return fmt.Errorf("last restored step=%d, want %d", lastUV, wantLastStep)
	}
	return nil
}

// attackSHIP01 kills the walship capture process via strace SIGKILL at every
// critical syscall during Open() (SHIP-01a) and during Cycle() (SHIP-01b: segment
// put, PASSIVE checkpoint, and periodic snapshot), then resumes and verifies full E1 restore.
func attackSHIP01(ctx context.Context, r recipe.Recipe, hashes []recipe.FileHashes, work string) {
	if !trace.Supported {
		report("SHIP-01a", "SIGKILL capture process during walship.Open and resume",
			"skipped on non-Linux", "LINUX-ONLY: strace unavailable on "+runtime.GOOS, true)
		report("SHIP-01b", "SIGKILL capture process at every point of walship.Cycle and resume",
			"skipped on non-Linux", "LINUX-ONLY: strace unavailable on "+runtime.GOOS, true)
		return
	}
	dir := filepath.Join(work, "ship01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship01 mkdir", err)
	}
	defer os.RemoveAll(dir)

	// --- SHIP-01a: Kill during Open() initial snapshot ---
	base2 := filepath.Join(dir, "base2.db")
	if err := r.Build(ctx, base2, 2); err != nil {
		fatal("ship01a build", err)
	}
	traceOpenDir := filepath.Join(dir, "open-trace")
	traceDB := filepath.Join(traceOpenDir, "r.db")
	traceStore := filepath.Join(traceOpenDir, "store")
	if err := copyFile(base2, traceDB); err != nil {
		fatal("ship01a copy", err)
	}
	cmd, err := trace.SelfCommand(modeShipOpen, "-db", traceDB, "-store", traceStore, "-every", "2", "-origin", "2")
	if err != nil {
		fatal("ship01a cmd", err)
	}
	res, err := trace.Run(ctx, trace.Options{Command: cmd})
	if err != nil || res.ExitCode != 0 {
		fatal("ship01a trace", fmt.Errorf("%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr))
	}
	realStore, _ := filepath.EvalSymlinks(traceStore)
	tid := mainThreadOf(res, realStore)

	// Find critical syscalls on the store dir during Open(): write to .tmp, fsync of .tmp, linkat/link, unlinkat, fsync of snapshots dir.
	var openPoints []trace.KillAt
	counts := map[string]int{}
	for _, c := range res.Log.Calls {
		if c.PID != tid {
			continue
		}
		counts[c.Name]++
		if !strings.HasPrefix(c.Path, realStore) {
			continue
		}
		switch c.Name {
		case "write", "fsync", "linkat", "link", "unlinkat", "unlink":
			openPoints = append(openPoints, trace.KillAt{Syscalls: []string{c.Name}, When: counts[c.Name]})
		}
	}

	openPassed := 0
	var openFail string
	for i, pt := range openPoints {
		kdir := filepath.Join(dir, fmt.Sprintf("open-kill-%02d", i))
		kdb := filepath.Join(kdir, "r.db")
		kstore := filepath.Join(kdir, "store")
		if err := copyFile(base2, kdb); err != nil {
			fatal("ship01a kcopy", err)
		}
		kcmd, _ := trace.SelfCommand(modeShipOpen, "-db", kdb, "-store", kstore, "-every", "2", "-origin", "2")
		kres, err := trace.Run(ctx, trace.Options{Command: kcmd, KillAt: &pt})
		if err != nil {
			fatal("ship01a krun", err)
		}
		// Resume with a fresh shipper, commit step 3, cycle, verify full E1 restore!
		ds, err := walship.NewDirStore(kstore)
		if err != nil {
			openFail = fmt.Sprintf("kill %d (%s#%d): NewDirStore: %v", i, pt.Syscalls[0], pt.When, err)
			continue
		}
		s, err := walship.Open(ctx, walship.Config{Database: kdb, Store: ds, Options: r.SQLiteOptions(), SnapshotEvery: 2, Origin: 2})
		if err != nil {
			openFail = fmt.Sprintf("kill %d (%s#%d): reopen: %v", i, pt.Syscalls[0], pt.When, err)
			continue
		}
		if err := applySessionStep(ctx, r, kdb, 3); err != nil {
			s.Close()
			openFail = fmt.Sprintf("kill %d: step 3: %v", i, err)
			continue
		}
		if _, err := s.Cycle(ctx); err != nil {
			s.Close()
			openFail = fmt.Sprintf("kill %d (%s#%d): cycle: %v", i, pt.Syscalls[0], pt.When, err)
			continue
		}
		_ = s.Close()
		if err := verifyStoreRestoresAll(ctx, ds, hashes, 2, 3); err != nil || kres.Killed != "SIGKILL" {
			if openFail == "" {
				openFail = fmt.Sprintf("kill %d (%s#%d): killed=%q verify=%v", i, pt.Syscalls[0], pt.When, kres.Killed, err)
			}
			continue
		}
		openPassed++
	}
	heldA := len(openPoints) > 0 && openPassed == len(openPoints)
	gotA := fmt.Sprintf("%d/%d SIGKILL points during walship.Open resumed cleanly and restored steps 2-3 E1", openPassed, len(openPoints))
	if openFail != "" {
		gotA += "; first=" + openFail
	}
	report("SHIP-01a", "SIGKILL capture process on store write/fsync/linkat/unlinkat during walship.Open, then resume",
		"no .tmp leaks, fresh shipper resumes cleanly, ships step 3, and restores all positions E1", gotA, heldA)

	// --- SHIP-01b: Kill during Cycle() (segment put + PASSIVE checkpoint + periodic snapshot) ---
	// Prepare a clean fixture: initial snapshot at 2, segment 3 shipped, step 4 committed in -wal,
	// SnapshotEvery=2 so the next Cycle() ships segment 4, checkpoints r.db, AND puts periodic snapshot 4!
	fixDir := filepath.Join(dir, "cycle-fixture")
	fixDB := filepath.Join(fixDir, "r.db")
	fixStoreDir := filepath.Join(fixDir, "store")
	if err := copyFile(base2, fixDB); err != nil {
		fatal("ship01b copy", err)
	}
	// Hold a keeper connection via procs so closing the setup shipper does not delete -wal.
	var scriptOut strings.Builder
	script := procs.NewScript(&scriptOut)
	defer script.Close()
	if _, err := script.Spawn("keeper", procs.RoleSQL, procs.SQLOptions{Path: fixDB, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}.Args()...); err != nil {
		fatal("ship01b keeper", err)
	}
	fixStore, err := walship.NewDirStore(fixStoreDir)
	if err != nil {
		fatal("ship01b store", err)
	}
	s0, err := walship.Open(ctx, walship.Config{Database: fixDB, Store: fixStore, Options: r.SQLiteOptions(), SnapshotEvery: 2, Origin: 2})
	if err != nil {
		fatal("ship01b open", err)
	}
	if err := applySessionStep(ctx, r, fixDB, 3); err != nil {
		fatal("ship01b step 3", err)
	}
	if _, err := s0.Cycle(ctx); err != nil {
		fatal("ship01b cycle 3", err)
	}
	if err := applySessionStep(ctx, r, fixDB, 4); err != nil {
		fatal("ship01b step 4", err)
	}
	// Close setup shipper (keeper still holds DB open so -wal with step 4 stays intact!).
	if err := s0.Close(); err != nil {
		fatal("ship01b close", err)
	}
	// Snapshot fixture files using copier child (lock-safe) before stopping keeper.
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", fixDB, "-out", filepath.Join(dir, "fix-copy"))
	if err != nil {
		fatal("ship01b copier", err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "fix-copy"), 0o755)
	if _, err := script.Do(copier, "copy", "db", "r.db"); err != nil {
		fatal("ship01b copy db", err)
	}
	if _, err := script.Do(copier, "copy", "-wal", "r.db-wal"); err != nil {
		fatal("ship01b copy wal", err)
	}
	_ = script.Close()

	cloneFixture := func(dst string) (string, string) {
		dbp := filepath.Join(dst, "r.db")
		stp := filepath.Join(dst, "store")
		if err := errors.Join(
			copyFile(filepath.Join(dir, "fix-copy", "r.db"), dbp),
			copyFile(filepath.Join(dir, "fix-copy", "r.db-wal"), dbp+"-wal"),
			copyDir(fixStoreDir, stp),
		); err != nil {
			fatal("ship01b clone", err)
		}
		return dbp, stp
	}

	// Trace an uninterrupted Cycle() on the fixture.
	tDB, tStore := cloneFixture(filepath.Join(dir, "cycle-trace"))
	tCmd, _ := trace.SelfCommand(modeShipCycle, "-db", tDB, "-store", tStore, "-every", "2", "-origin", "2")
	tRes, err := trace.Run(ctx, trace.Options{Command: tCmd})
	if err != nil || tRes.ExitCode != 0 {
		fatal("ship01b trace", fmt.Errorf("%v exit=%d stderr=%s", err, tRes.ExitCode, tRes.Stderr))
	}
	realTStore, _ := filepath.EvalSymlinks(tStore)
	realTDB, _ := filepath.EvalSymlinks(tDB)
	ctid := mainThreadOf(tRes, realTStore)

	// Collect kill points: all store write/fsync/linkat/unlinkat calls, PLUS early/mid/last
	// r.db backfill pwrite64 and r.db fsync during PASSIVE checkpoint!
	var cyclePoints []trace.KillAt
	var dbPwrites []int
	cCounts := map[string]int{}
	for _, c := range tRes.Log.Calls {
		if c.PID != ctid {
			continue
		}
		cCounts[c.Name]++
		if strings.HasPrefix(c.Path, realTStore) {
			switch c.Name {
			case "write", "fsync", "linkat", "link", "unlinkat", "unlink":
				cyclePoints = append(cyclePoints, trace.KillAt{Syscalls: []string{c.Name}, When: cCounts[c.Name]})
			}
		} else if c.Path == realTDB {
			switch c.Name {
			case "pwrite64":
				dbPwrites = append(dbPwrites, cCounts[c.Name])
			case "fsync":
				cyclePoints = append(cyclePoints, trace.KillAt{Syscalls: []string{"fsync"}, When: cCounts[c.Name]})
			}
		}
	}
	if len(dbPwrites) > 0 {
		cyclePoints = append(cyclePoints,
			trace.KillAt{Syscalls: []string{"pwrite64"}, When: dbPwrites[0]},
			trace.KillAt{Syscalls: []string{"pwrite64"}, When: dbPwrites[len(dbPwrites)/2]},
			trace.KillAt{Syscalls: []string{"pwrite64"}, When: dbPwrites[len(dbPwrites)-1]},
		)
	}

	cyclePassed := 0
	var cycleFail string
	for i, pt := range cyclePoints {
		kdb, kstore := cloneFixture(filepath.Join(dir, fmt.Sprintf("cycle-kill-%02d", i)))
		kcmd, _ := trace.SelfCommand(modeShipCycle, "-db", kdb, "-store", kstore, "-every", "2", "-origin", "2")
		kres, err := trace.Run(ctx, trace.Options{Command: kcmd, KillAt: &pt})
		if err != nil {
			fatal("ship01b krun", err)
		}
		ds, err := walship.NewDirStore(kstore)
		if err != nil {
			cycleFail = fmt.Sprintf("kill %d: NewDirStore: %v", i, err)
			continue
		}
		s, err := walship.Open(ctx, walship.Config{Database: kdb, Store: ds, Options: r.SQLiteOptions(), SnapshotEvery: 2, Origin: 2})
		if err != nil {
			if cycleFail == "" {
				cycleFail = fmt.Sprintf("kill %d (%s#%d): reopen: %v", i, pt.Syscalls[0], pt.When, err)
			}
			continue
		}
		// Run a recovery cycle, then commit step 5 and run another cycle.
		if _, err := s.Cycle(ctx); err != nil {
			s.Close()
			if cycleFail == "" {
				cycleFail = fmt.Sprintf("kill %d (%s#%d): resume cycle: %v", i, pt.Syscalls[0], pt.When, err)
			}
			continue
		}
		if err := applySessionStep(ctx, r, kdb, 5); err != nil {
			s.Close()
			if cycleFail == "" {
				cycleFail = fmt.Sprintf("kill %d: step 5: %v", i, err)
			}
			continue
		}
		if _, err := s.Cycle(ctx); err != nil {
			s.Close()
			if cycleFail == "" {
				cycleFail = fmt.Sprintf("kill %d (%s#%d): step 5 cycle: %v", i, pt.Syscalls[0], pt.When, err)
			}
			continue
		}
		_ = s.Close()
		if err := verifyStoreRestoresAll(ctx, ds, hashes, 2, 5); err != nil || kres.Killed != "SIGKILL" {
			if cycleFail == "" {
				cycleFail = fmt.Sprintf("kill %d (%s#%d): killed=%q verify=%v", i, pt.Syscalls[0], pt.When, kres.Killed, err)
			}
			continue
		}
		cyclePassed++
	}
	heldB := len(cyclePoints) > 0 && cyclePassed == len(cyclePoints)
	gotB := fmt.Sprintf("%d/%d SIGKILL points during Cycle() (segment put, PASSIVE checkpoint r.db writes/fsync, periodic snapshot put) resumed cleanly and restored all positions through step 5 E1", cyclePassed, len(cyclePoints))
	if cycleFail != "" {
		gotB += "; first=" + cycleFail
	}
	report("SHIP-01b", "SIGKILL capture process at every store & checkpoint syscall inside walship.Cycle, then resume",
		"resumed shipper recovers without collision/corruption, ships step 5, and restores every position E1", gotB, heldB)
}

// attackSHIP02 tests outside checkpoints behind the shipper's back in all three
// regimes: (a) live shipper holding slot 0, (b) live shipper holding slot >= 1,
// (c) stopped shipper + outside RESTART checkpoint (WAL non-empty).
func attackSHIP02(ctx context.Context, r recipe.Recipe, hashes []recipe.FileHashes, work string) {
	dir := filepath.Join(work, "ship02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship02 mkdir", err)
	}
	defer os.RemoveAll(dir)

	// --- SHIP-02a: Live shipper holding pin on slot 0 ---
	dbA := filepath.Join(dir, "a", "r.db")
	if err := os.MkdirAll(filepath.Dir(dbA), 0o755); err != nil {
		fatal("ship02a mkdir", err)
	}
	if err := r.Build(ctx, dbA, 2); err != nil {
		fatal("ship02a build", err)
	}
	storeA, _ := walship.NewDirStore(filepath.Join(dir, "a", "store"))
	var outA strings.Builder
	scriptA := procs.NewScript(&outA)
	defer scriptA.Close()
	optsA := procs.SQLOptions{Path: dbA, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}
	outsiderA, err := scriptA.Spawn("outsider", procs.RoleSQL, optsA.Args()...)
	if err != nil {
		fatal("ship02a outsider", err)
	}
	sA, err := walship.Open(ctx, walship.Config{Database: dbA, Store: storeA, Options: r.SQLiteOptions(), SnapshotEvery: 0, Origin: 2})
	if err != nil {
		fatal("ship02a open", err)
	}
	// Step 3 -> shipped & fully backfilled -> shipper pins slot 0.
	if err := applySessionStep(ctx, r, dbA, 3); err != nil {
		fatal("ship02a step3", err)
	}
	rep3, err := sA.Cycle(ctx)
	if err != nil || !rep3.FullBackfill {
		fatal("ship02a cycle3", fmt.Errorf("err=%v full=%t", err, rep3.FullBackfill))
	}
	// Part 1: Outsider runs TRUNCATE right after full backfill (succeeds because nBackfill==mxFrame, wal.c:2367).
	trRes, err := scriptA.Do(outsiderA, "checkpoint", sqlitex.CheckpointTruncate)
	if err != nil {
		fatal("ship02a truncate", err)
	}
	// Writer commits step 4; shipper cycles -> must NOT lose step 4!
	if err := applySessionStep(ctx, r, dbA, 4); err != nil {
		fatal("ship02a step4", err)
	}
	rep4, err := sA.Cycle(ctx)
	if err != nil {
		fatal("ship02a cycle4", err)
	}
	// Part 2: Writer commits step 5 (unshipped frames in WAL while shipper STILL holds slot 0!).
	if err := applySessionStep(ctx, r, dbA, 5); err != nil {
		fatal("ship02a step5", err)
	}
	// Outsider tries all four checkpoint modes with busy_timeout=0: NONE may backfill step 5!
	blockedModes := 0
	var modeSummary []string
	for _, mode := range []string{sqlitex.CheckpointPassive, sqlitex.CheckpointFull, sqlitex.CheckpointRestart, sqlitex.CheckpointTruncate} {
		cr, cerr := scriptA.Do(outsiderA, "checkpoint", mode)
		if cerr != nil {
			fatal("ship02a ckpt "+mode, cerr)
		}
		// Must not have checkpointed any frame (checkpointed=0) or must report busy=1.
		if strings.Contains(cr.Value, "checkpointed=0") {
			blockedModes++
		}
		modeSummary = append(modeSummary, mode+":"+cr.Value)
	}
	rep5, err := sA.Cycle(ctx)
	if err != nil {
		fatal("ship02a cycle5", err)
	}
	_ = sA.Close()
	_ = scriptA.Close()
	verA := verifyStoreRestoresAll(ctx, storeA, hashes, 2, 5)
	heldA := blockedModes == 4 && rep4.Segment != nil && rep5.Segment != nil && verA == nil
	report("SHIP-02a", "outside checkpoints behind a LIVE shipper pinned on slot 0 (after backfill & with unshipped step 5)",
		"post-backfill TRUNCATE is harmless; unshipped step 5 cannot be backfilled by PASSIVE/FULL/RESTART/TRUNCATE; all steps restore E1",
		fmt.Sprintf("postBackfillTrunc=%q; unshipped blocked=%d/4 (%s); verify=%v", trRes.Value, blockedModes, strings.Join(modeSummary, ", "), verA), heldA)

	// --- SHIP-02b: Live shipper holding pin on slot >= 1 (concurrent reader blocked backfill) ---
	dbB := filepath.Join(dir, "b", "r.db")
	if err := os.MkdirAll(filepath.Dir(dbB), 0o755); err != nil {
		fatal("ship02b mkdir", err)
	}
	if err := r.Build(ctx, dbB, 2); err != nil {
		fatal("ship02b build", err)
	}
	storeB, _ := walship.NewDirStore(filepath.Join(dir, "b", "store"))
	var outB strings.Builder
	scriptB := procs.NewScript(&outB)
	defer scriptB.Close()
	readerB, err := scriptB.Spawn("reader", procs.RoleSQL, procs.SQLOptions{Path: dbB, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}.Args()...)
	if err != nil {
		fatal("ship02b reader", err)
	}
	outsiderB, err := scriptB.Spawn("outsider", procs.RoleSQL, procs.SQLOptions{Path: dbB, JournalMode: sqlitex.JournalWAL, BusyTimeout: 0}.Args()...)
	if err != nil {
		fatal("ship02b outsider", err)
	}
	sB, err := walship.Open(ctx, walship.Config{Database: dbB, Store: storeB, Options: r.SQLiteOptions(), SnapshotEvery: 0, Origin: 2})
	if err != nil {
		fatal("ship02b open", err)
	}
	// Step 3 -> reader begins read txn at step 3 -> step 4 -> shipper ships 3..4 (partial backfill! pin takes slot >= 1).
	if err := applySessionStep(ctx, r, dbB, 3); err != nil {
		fatal("ship02b step3", err)
	}
	_, _ = scriptB.Do(readerB, "begin", "deferred")
	_, _ = scriptB.Do(readerB, "query", "SELECT count(*) FROM accounts")
	if err := applySessionStep(ctx, r, dbB, 4); err != nil {
		fatal("ship02b step4", err)
	}
	repB4, err := sB.Cycle(ctx)
	if err != nil || repB4.FullBackfill {
		fatal("ship02b cycle4", fmt.Errorf("err=%v full=%t (want partial backfill)", err, repB4.FullBackfill))
	}
	// Reader releases its lock! Now ONLY the shipper's slot>=1 pin remains.
	_, _ = scriptB.Do(readerB, "commit")
	// Outsider runs PASSIVE (backfills up to shipper's mark), then tries RESTART and TRUNCATE (must be blocked by shipper's slot>=1 pin!).
	pRes, _ := scriptB.Do(outsiderB, "checkpoint", sqlitex.CheckpointPassive)
	rRes, _ := scriptB.Do(outsiderB, "checkpoint", sqlitex.CheckpointRestart)
	tRes, _ := scriptB.Do(outsiderB, "checkpoint", sqlitex.CheckpointTruncate)
	// Step 5 must still land in the SAME WAL generation (no restart!).
	if err := applySessionStep(ctx, r, dbB, 5); err != nil {
		fatal("ship02b step5", err)
	}
	repB5, err := sB.Cycle(ctx)
	if err != nil {
		fatal("ship02b cycle5", err)
	}
	_ = sB.Close()
	_ = scriptB.Close()
	verB := verifyStoreRestoresAll(ctx, storeB, hashes, 2, 5)
	heldB := strings.Contains(rRes.Value, "busy=1") && strings.Contains(tRes.Value, "busy=1") &&
		repB5.Change == walship.WALUnchanged && repB5.Segment != nil && verB == nil
	report("SHIP-02b", "outside PASSIVE/RESTART/TRUNCATE behind a LIVE shipper pinned on slot>=1 (after concurrent reader closes)",
		"RESTART and TRUNCATE return busy=1; WAL stays in same generation; step 5 ships and all steps restore E1",
		fmt.Sprintf("PASSIVE=%q RESTART=%q TRUNCATE=%q nextChange=%q verify=%v", pRes.Value, rRes.Value, tRes.Value, repB5.Change, verB), heldB)

	// --- SHIP-02c: Stopped shipper + outside RESTART checkpoint (WAL stays non-empty!) ---
	dbC := filepath.Join(dir, "c", "r.db")
	if err := os.MkdirAll(filepath.Dir(dbC), 0o755); err != nil {
		fatal("ship02c mkdir", err)
	}
	if err := r.Build(ctx, dbC, 2); err != nil {
		fatal("ship02c build", err)
	}
	storeC, _ := walship.NewDirStore(filepath.Join(dir, "c", "store"))
	var outC strings.Builder
	scriptC := procs.NewScript(&outC)
	defer scriptC.Close()
	outsiderC, err := scriptC.Spawn("outsider", procs.RoleSQL, procs.SQLOptions{Path: dbC, JournalMode: sqlitex.JournalWAL, BusyTimeout: 5 * time.Second}.Args()...)
	if err != nil {
		fatal("ship02c outsider", err)
	}
	sC, err := walship.Open(ctx, walship.Config{Database: dbC, Store: storeC, Options: r.SQLiteOptions(), SnapshotEvery: 0, Origin: 2})
	if err != nil {
		fatal("ship02c open", err)
	}
	if err := applySessionStep(ctx, r, dbC, 3); err != nil {
		fatal("ship02c step3", err)
	}
	if _, err := sC.Cycle(ctx); err != nil {
		fatal("ship02c cycle3", err)
	}
	_ = sC.Close()

	// Shipper is down: writer commits 4, 5; outsider runs RESTART (NOT TRUNCATE -> WAL remains non-empty!); writer commits 6.
	if err := applySessionStep(ctx, r, dbC, 4); err != nil {
		fatal("ship02c step4", err)
	}
	if err := applySessionStep(ctx, r, dbC, 5); err != nil {
		fatal("ship02c step5", err)
	}
	rstRes, err := scriptC.Do(outsiderC, "checkpoint", sqlitex.CheckpointRestart)
	if err != nil {
		fatal("ship02c restart", err)
	}
	walInfo, _ := os.Stat(dbC + "-wal")
	if err := applySessionStep(ctx, r, dbC, 6); err != nil {
		fatal("ship02c step6", err)
	}
	sC, err = walship.Open(ctx, walship.Config{Database: dbC, Store: storeC, Options: r.SQLiteOptions(), SnapshotEvery: 0, Origin: 2})
	if err != nil {
		fatal("ship02c reopen", err)
	}
	repC, err := sC.Cycle(ctx)
	if err != nil {
		fatal("ship02c cycle after restart", err)
	}
	_ = sC.Close()
	_ = scriptC.Close()

	catC, err := walship.LoadCatalog(ctx, storeC)
	if err != nil {
		fatal("ship02c catalog", err)
	}
	_, gapErr := catC.PlanFrom(2, 4)
	verC := verifyStoreRestoresAll(ctx, storeC, hashes, 2, 6)
	heldC := walInfo != nil && walInfo.Size() > 0 &&
		repC.Change == walship.WALDiscontinuity &&
		repC.Snapshot != nil && repC.Snapshot.Reason == walship.ReasonDiscontinuity &&
		repC.Snapshot.UserVersion == 6 && repC.Segment == nil &&
		errors.Is(gapErr, walship.ErrBrokenChain) && verC == nil
	report("SHIP-02c", "outside RESTART checkpoint (non-empty WAL) while shipper is down, then commit 6 & resume",
		"shipper detects salt change as discontinuity, snapshots step 6, and PlanFrom across gap fails with ErrBrokenChain",
		fmt.Sprintf("RESTART=%q walBytes=%d change=%q snapUV=%d gapErr=%v verify=%v",
			rstRes.Value, walInfo.Size(), repC.Change, repC.Snapshot.UserVersion, gapErr, verC), heldC)
}

// attackSHIP03 tests six distinct segment and snapshot corruption attacks on internal/walship.
func attackSHIP03(ctx context.Context, r recipe.Recipe, hashes []recipe.FileHashes, work string) {
	dir := filepath.Join(work, "ship03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship03 mkdir", err)
	}
	defer os.RemoveAll(dir)

	// Build a clean baseline store with snapshots at 2 and 6, and a multi-commit segment at 4 (positions 4..5).
	dbPath := filepath.Join(dir, "live", "r.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fatal("ship03 live mkdir", err)
	}
	if err := r.Build(ctx, dbPath, 2); err != nil {
		fatal("ship03 build", err)
	}
	baseStore, err := walship.NewDirStore(filepath.Join(dir, "base-store"))
	if err != nil {
		fatal("ship03 base store", err)
	}
	s, err := walship.Open(ctx, walship.Config{Database: dbPath, Store: baseStore, Options: r.SQLiteOptions(), SnapshotEvery: 3, Origin: 2})
	if err != nil {
		fatal("ship03 open", err)
	}
	batches := [][]int{{3}, {4, 5}, {6}, {7}, {8}}
	for _, b := range batches {
		for _, k := range b {
			if err := applySessionStep(ctx, r, dbPath, k); err != nil {
				fatal("ship03 step", err)
			}
		}
		if _, err := s.Cycle(ctx); err != nil {
			fatal("ship03 cycle", err)
		}
	}
	if err := s.Close(); err != nil {
		fatal("ship03 close", err)
	}

	cloneStore := func(name string) *walship.DirStore {
		dst, err := walship.NewDirStore(filepath.Join(dir, "store-"+name))
		if err != nil {
			fatal("ship03 clone "+name, err)
		}
		if _, err := walship.CopyStore(ctx, baseStore, dst); err != nil {
			fatal("ship03 copy "+name, err)
		}
		return dst
	}
	replaceObj := func(st walship.Store, key string, data []byte) {
		if err := st.Delete(ctx, key); err != nil {
			fatal("replace delete", err)
		}
		if err := st.PutIfAbsent(ctx, key, data); err != nil {
			fatal("replace put", err)
		}
	}

	// SHIP-03a: Raw 1-bit flip in segment 7 body -> refused in Plan(8) with ErrBrokenChain.
	stA := cloneStore("a")
	raw7, err := stA.Get(ctx, walship.SegmentKey(7))
	if err != nil {
		fatal("ship03a get", err)
	}
	raw7[len(raw7)-100] ^= 0x01
	replaceObj(stA, walship.SegmentKey(7), raw7)
	catA, _ := walship.LoadCatalog(ctx, stA)
	_, errA := catA.Plan(8)
	report("SHIP-03a", "flip 1 bit in segment 7 body",
		"LoadCatalog marks damaged; Plan(8) refuses with ErrBrokenChain before replay",
		fmt.Sprintf("damaged=%d planErr=%v", len(catA.Damaged), errA),
		len(catA.Damaged) == 1 && errors.Is(errA, walship.ErrBrokenChain))

	// SHIP-03b: Flip 1 bit in segment 7 body AND recompute BodySHA256 -> caught by WAL checksum chain in LoadCatalog/Plan(8).
	stB := cloneStore("b")
	orig7B, err := stB.Get(ctx, walship.SegmentKey(7))
	if err != nil {
		fatal("ship03b get", err)
	}
	meta7, frames7, err := walship.DecodeSegment(orig7B)
	if err != nil {
		fatal("ship03b decode", err)
	}
	frames7[len(frames7)-100] ^= 0x01
	enc7B, _ := walship.EncodeSegment(meta7, frames7)
	replaceObj(stB, walship.SegmentKey(7), enc7B)
	catB, _ := walship.LoadCatalog(ctx, stB)
	_, errB := catB.Plan(8)
	report("SHIP-03b", "flip 1 bit in segment 7 body + recompute BodySHA256",
		"VerifySegmentFrames catches WAL checksum chain mismatch during LoadCatalog/Plan(8)",
		fmt.Sprintf("damaged=%d planErr=%v", len(catB.Damaged), errB),
		len(catB.Damaged) == 1 && errors.Is(errB, walship.ErrBrokenChain) && strings.Contains(errB.Error(), "checksum"))

	// SHIP-03c: Flip 1 bit in segment 7 page AND forge BOTH BodySHA256 AND the WAL frame checksum chain + LastChecksum,
	// leaving original AfterSHA256 -> Plan accepts, Execute() catches state hash mismatch!
	stC := cloneStore("c")
	orig7C, err := stC.Get(ctx, walship.SegmentKey(7))
	if err != nil {
		fatal("ship03c get", err)
	}
	meta7C, frames7C, err := walship.DecodeSegment(orig7C)
	if err != nil {
		fatal("ship03c decode", err)
	}
	hdr7, _ := meta7C.Generation.Bytes()
	bigEndian := (uint32(hdr7[0])<<24|uint32(hdr7[1])<<16|uint32(hdr7[2])<<8|uint32(hdr7[3]))&1 == 1
	frameSize := meta7C.PageSize + walfile.FrameHeaderSize
	// Flip 1 bit in the page payload of the last frame.
	frames7C[len(frames7C)-50] ^= 0x01
	// Recompute the entire WAL frame checksum chain from SeedChecksum.
	chain := walfile.Checksum(meta7C.SeedChecksum)
	for off := 0; off < len(frames7C); off += frameSize {
		fr := frames7C[off : off+frameSize]
		next, _ := walfile.ChecksumBytes(bigEndian, fr[:8], chain)
		next, _ = walfile.ChecksumBytes(bigEndian, fr[walfile.FrameHeaderSize:], next)
		fr[16], fr[17], fr[18], fr[19] = byte(next[0]>>24), byte(next[0]>>16), byte(next[0]>>8), byte(next[0])
		fr[20], fr[21], fr[22], fr[23] = byte(next[1]>>24), byte(next[1]>>16), byte(next[1]>>8), byte(next[1])
		chain = next
	}
	meta7C.LastChecksum = chain
	enc7C, _ := walship.EncodeSegment(meta7C, frames7C)
	replaceObj(stC, walship.SegmentKey(7), enc7C)
	catC, _ := walship.LoadCatalog(ctx, stC)
	plan7C, planErrC := catC.Plan(7)
	var execErrC error
	if planErrC == nil {
		_, _, execErrC = plan7C.Execute()
	}
	report("SHIP-03c", "flip page bit + forge BodySHA256 + forge entire WAL checksum chain & LastChecksum (keep AfterSHA256)",
		"Execute() detects tampered page via AfterSHA256 state hash check (ErrBrokenChain)",
		fmt.Sprintf("planErr=%v execErr=%v", planErrC, execErrC),
		planErrC == nil && errors.Is(execErrC, walship.ErrBrokenChain))

	// SHIP-03d: Tamper multi-commit segment 4 (positions 4..5) so Commits[0].Frame points to
	// frame 1 (a NON-commit frame inside transaction 4!), with valid BodySHA256 and valid WAL checksum chain.
	// Build-notes / 9.4 claim: chain and commit validation run while planning, before any page is applied.
	stD := cloneStore("d")
	orig4, _ := stD.Get(ctx, walship.SegmentKey(4))
	meta4, frames4, err := walship.DecodeSegment(orig4)
	if err != nil {
		fatal("ship03d decode", err)
	}
	if len(meta4.Commits) != 2 || meta4.Commits[0].Frame <= 1 {
		fatal("ship03d fixture", fmt.Errorf("expected 2 commits with commit[0].Frame>1, got %+v", meta4.Commits))
	}
	// Point commit position 4 to frame 1 (which has dbSize=0: NOT a commit frame!).
	meta4.Commits = append([]walship.CommitPoint(nil), meta4.Commits...)
	meta4.Commits[0].Frame = meta4.FirstFrame
	enc4D, _ := walship.EncodeSegment(meta4, frames4)
	replaceObj(stD, walship.SegmentKey(4), enc4D)
	catD, _ := walship.LoadCatalog(ctx, stD)
	plan4D, planErrD := catD.Plan(4)
	var execErrD error
	if planErrD == nil {
		_, _, execErrD = plan4D.Execute()
	}
	// If LoadCatalog + Plan(4) catches the non-commit CommitPoint.Frame before any page is applied, HELD.
	// If Plan(4) accepts it and only Execute() fails, BROKEN!
	caughtAtPlan := planErrD != nil
	report("SHIP-03d", "tamper segment 4 metadata Commits[0].Frame to non-commit frame 1 (valid body hash & WAL checksum chain)",
		"LoadCatalog/Plan(4) rejects non-commit CommitPoint.Frame before any page is applied",
		fmt.Sprintf("damaged=%d planErr=%v execErr=%v", len(catD.Damaged), planErrD, execErrD), caughtAtPlan)

	// SHIP-03e: Corrupt periodic snapshot 6 -> LoadCatalog marks damaged, Plan(8) falls back to snapshot 2 and restores E1!
	stE := cloneStore("e")
	rawSnap6, _ := stE.Get(ctx, walship.SnapshotKey(6))
	rawSnap6[len(rawSnap6)-100] ^= 0x01
	replaceObj(stE, walship.SnapshotKey(6), rawSnap6)
	catE, _ := walship.LoadCatalog(ctx, stE)
	plan8E, planErrE := catE.Plan(8)
	var img8E []byte
	var execErrE error
	if planErrE == nil {
		img8E, _, execErrE = plan8E.Execute()
	}
	heldE := len(catE.Damaged) == 1 && planErrE == nil && execErrE == nil &&
		plan8E.Snapshot.Meta.Position == 2 && walship.SHA256Hex(img8E) == hashes[8].Exact
	report("SHIP-03e", "corrupt periodic snapshot 6 while snapshot 2 and segments 3..8 remain intact",
		"LoadCatalog marks snapshot 6 damaged; Plan(8) falls back to snapshot 2 and restores step 8 E1",
		fmt.Sprintf("damaged=%d fallbackSnap=%d planErr=%v execErr=%v e1=%t",
			len(catE.Damaged), plan8E.Snapshot.Meta.Position, planErrE, execErrE, walship.SHA256Hex(img8E) == hashes[8].Exact), heldE)

	// SHIP-03f: Corrupt the LATEST segment (segment 8) in the store, then call walship.Open() + Cycle() to resume!
	// Does Open() detect the damaged store at the tip, or silently resume at position 7 and fail in Cycle() with ErrExists?
	stF := cloneStore("f")
	dbF := filepath.Join(dir, "live-f", "r.db")
	if err := copyFile(dbPath, dbF); err != nil {
		fatal("ship03f copy db", err)
	}
	raw8, err := stF.Get(ctx, walship.SegmentKey(8))
	if err != nil {
		fatal("ship03f get 8", err)
	}
	raw8[len(raw8)-100] ^= 0x01
	replaceObj(stF, walship.SegmentKey(8), raw8)
	sF, openErrF := walship.Open(ctx, walship.Config{Database: dbF, Store: stF, Options: r.SQLiteOptions(), SnapshotEvery: 3, Origin: 2})
	var cycErrF error
	var resumedPos int64 = -1
	if openErrF == nil {
		resumedPos = sF.Position()
		if err := applySessionStep(ctx, r, dbF, 9); err == nil {
			_, cycErrF = sF.Cycle(ctx)
		}
		_ = sF.Close()
	}
	// If Open() refused to resume silently past a damaged latest segment, HELD; if Open() succeeded at pos 7
	// and then Cycle() collided on segments/000000000008 (or snapshots/000000000008) with ErrExists, BROKEN!
	heldF := openErrF != nil
	report("SHIP-03f", "corrupt latest segment 8 in store, then call walship.Open() to resume and ship step 9",
		"walship.Open() detects damaged latest segment on resume instead of silently rewinding to position 7",
		fmt.Sprintf("openErr=%v resumedPos=%d nextCycleErr=%v (isErrExists=%t)",
			openErrF, resumedPos, cycErrF, errors.Is(cycErrF, walship.ErrExists)), heldF)
}

// attackSHIP04 verifies that restoring inside a multi-statement transaction is
// impossible, and that rolled-back spilled frames and crashed-writer uncommitted
// frames are never shipped or visible in any restore point.
func attackSHIP04(ctx context.Context, self string, r recipe.Recipe, work string) {
	dir := filepath.Join(work, "ship04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship04 mkdir", err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, dbPath, 2); err != nil {
		fatal("ship04 build", err)
	}
	store, err := walship.NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		fatal("ship04 store", err)
	}
	s, err := walship.Open(ctx, walship.Config{Database: dbPath, Store: store, Options: r.SQLiteOptions(), SnapshotEvery: 0, Origin: 2})
	if err != nil {
		fatal("ship04 open", err)
	}

	// 1. Multi-statement transaction: 3 distinct INSERT statements in one COMMIT.
	wOpts := r.SQLiteOptions()
	wOpts.SingleConnection = true
	wOpts.ImmediateTx = false
	wOpts.Conn.WALAutocheckpoint = 0
	wOpts.Conn.CacheSize = 2
	wOpts.Conn.BusyTimeout = 5 * time.Second
	wdb, err := sqlitex.Open(ctx, dbPath, wOpts)
	if err != nil {
		fatal("ship04 open writer", err)
	}
	wconn, err := wdb.Conn(ctx)
	if err != nil {
		fatal("ship04 wconn", err)
	}
	if _, err := wconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		fatal("ship04 begin1", err)
	}
	for _, id := range []int{800001, 800002, 800003} {
		if _, err := wconn.ExecContext(ctx, "INSERT INTO events(id, account_id, kind, amount, note) VALUES (?, 1, 'multi', 10, 'inside-txn')", id); err != nil {
			fatal("ship04 insert", err)
		}
	}
	if _, err := wconn.ExecContext(ctx, "COMMIT"); err != nil {
		fatal("ship04 commit1", err)
	}
	repMulti, err := s.Cycle(ctx)
	if err != nil {
		fatal("ship04 cycle multi", err)
	}

	// 2. Large transaction with cache_size=2 that spills many frames to -wal, then executes ROLLBACK!
	if _, err := wconn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		fatal("ship04 begin2", err)
	}
	big := strings.Repeat("R", 3500)
	for i := 1; i <= 35; i++ {
		if _, err := wconn.ExecContext(ctx, "INSERT INTO events(id, account_id, kind, amount, note) VALUES (?, 1, 'rolledback', -99, ?)", 850000+i, big); err != nil {
			fatal("ship04 spill insert", err)
		}
	}
	walInfoBeforeRB, _ := os.Stat(dbPath + "-wal")
	if _, err := wconn.ExecContext(ctx, "ROLLBACK"); err != nil {
		fatal("ship04 rollback", err)
	}
	wconn.Close()
	wdb.Close()

	repRB, err := s.Cycle(ctx)
	if err != nil {
		fatal("ship04 cycle rollback", err)
	}

	// 3. Crashed writer leaving spilled uncommitted frames in -wal (child exits via os.Exit(0) mid-txn).
	if out, err := runCommand(self, modeSpillAndCrash, "-db", dbPath); err != nil {
		fatal("ship04 spill crash", fmt.Errorf("%v: %s", err, out))
	}
	walInfoAfterCrash, _ := os.Stat(dbPath + "-wal")
	repCrash, err := s.Cycle(ctx)
	if err != nil {
		fatal("ship04 cycle crash", err)
	}
	_ = s.Close()

	// Verify catalog: only positions 2 (initial) and 3 (the single multi-statement commit) exist!
	cat, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		fatal("ship04 catalog", err)
	}
	lastPos, _ := cat.LastPosition()
	_, errPos4 := cat.Plan(4)

	countMarker := func(pos int64) (multiRows, rbRows, crashRows int, integ string) {
		plan, err := cat.Plan(pos)
		if err != nil {
			fatal("ship04 plan", err)
		}
		img, _, err := plan.Execute()
		if err != nil {
			fatal("ship04 exec", err)
		}
		outDB := filepath.Join(dir, fmt.Sprintf("restored-%d.db", pos))
		if err := walship.WriteDatabase(outDB, img); err != nil {
			fatal("ship04 write", err)
		}
		db, err := r.Open(ctx, outDB)
		if err != nil {
			fatal("ship04 open restored", err)
		}
		defer db.Close()
		_ = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integ)
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind='multi'").Scan(&multiRows)
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind='rolledback'").Scan(&rbRows)
		_ = db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind='crash'").Scan(&crashRows)
		return
	}

	m2, rb2, cr2, i2 := countMarker(2)
	m3, rb3, cr3, i3 := countMarker(3)

	held := repMulti.Segment != nil && len(repMulti.Segment.Commits) == 1 &&
		repRB.Segment == nil && repCrash.Segment == nil &&
		lastPos == 3 && errors.Is(errPos4, walship.ErrBrokenChain) &&
		walInfoBeforeRB != nil && walInfoBeforeRB.Size() > 50000 &&
		walInfoAfterCrash != nil && walInfoAfterCrash.Size() > 50000 &&
		i2 == "ok" && m2 == 0 && rb2 == 0 && cr2 == 0 &&
		i3 == "ok" && m3 == 3 && rb3 == 0 && cr3 == 0

	got := fmt.Sprintf("multiCommits=%d lastPos=%d plan(4)=%v; spillWAL=%dB rbSeg=%v; crashWAL=%dB crashSeg=%v; pos2=(multi=%d rb=%d crash=%d) pos3=(multi=%d rb=%d crash=%d)",
		len(repMulti.Segment.Commits), lastPos, errPos4, walInfoBeforeRB.Size(), repRB.Segment != nil, walInfoAfterCrash.Size(), repCrash.Segment != nil, m2, rb2, cr2, m3, rb3, cr3)
	report("SHIP-04", "multi-statement txn (3 inserts/1 commit), spilled ROLLBACK (cache_size=2), and crashed uncommitted spill",
		"only whole commit points exist (0 or 3 rows, never 1 or 2); rolled-back and crashed uncommitted frames are never shipped", got, held)
}

func runCommand(bin string, args ...string) ([]byte, error) {
	return exec.Command(bin, args...).CombinedOutput()
}

// attackSHIP05 tests a shrinking database under walship (DELETE 90% + VACUUM) with
// SnapshotEvery=1 so periodic snapshots verify the replayed truncated image against
// sqlite_dbpage (ErrMirrorDiverged check).
func attackSHIP05(ctx context.Context, r recipe.Recipe, work string) {
	dir := filepath.Join(work, "ship05")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship05 mkdir", err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, dbPath, 12); err != nil {
		fatal("ship05 build", err)
	}
	origStat, _ := os.Stat(dbPath)
	store, err := walship.NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		fatal("ship05 store", err)
	}
	s, err := walship.Open(ctx, walship.Config{Database: dbPath, Store: store, Options: r.SQLiteOptions(), SnapshotEvery: 1, Origin: 12})
	if err != nil {
		fatal("ship05 open", err)
	}

	// Open a writer, delete 90% of events, commit, cycle; then run plain VACUUM (shrinking the DB file!), cycle!
	wOpts := r.SQLiteOptions()
	wOpts.SingleConnection = true
	wOpts.ImmediateTx = false
	wOpts.Conn.WALAutocheckpoint = 0
	wOpts.Conn.BusyTimeout = 5 * time.Second
	wdb, err := sqlitex.Open(ctx, dbPath, wOpts)
	if err != nil {
		fatal("ship05 wopen", err)
	}
	if _, err := wdb.ExecContext(ctx, "DELETE FROM events WHERE id % 10 != 0; DELETE FROM documents;"); err != nil {
		fatal("ship05 delete", err)
	}
	repDel, err := s.Cycle(ctx)
	if err != nil {
		fatal("ship05 cycle del", err)
	}
	if _, err := wdb.ExecContext(ctx, "VACUUM"); err != nil {
		fatal("ship05 vacuum", err)
	}
	wdb.Close()

	repVac, cycVacErr := s.Cycle(ctx)
	_ = s.Close()

	finalLive, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("ship05 read final live", err)
	}

	if cycVacErr != nil {
		report("SHIP-05", "shrinking database under walship (DELETE 90% + plain VACUUM with SnapshotEvery=1 mirror check)",
			"walship replays shrinking WAL commit, passes periodic mirror check, and restores exact E1 shrunk DB",
			fmt.Sprintf("Cycle after VACUUM failed: %v", cycVacErr), false)
		return
	}

	cat, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		fatal("ship05 catalog", err)
	}
	last, _ := cat.LastPosition()
	// Restore from initial snapshot (12) all the way through DELETE + VACUUM!
	planFromInit, err := cat.PlanFrom(12, last)
	if err != nil {
		fatal("ship05 planFrom", err)
	}
	imgFromInit, _, err := planFromInit.Execute()
	if err != nil {
		fatal("ship05 exec", err)
	}
	restoredPath := filepath.Join(dir, "restored.db")
	if err := walship.WriteDatabase(restoredPath, imgFromInit); err != nil {
		fatal("ship05 write", err)
	}
	insp, err := procs.InspectDatabase(ctx, restoredPath, sqlitex.JournalWAL)
	if err != nil {
		fatal("ship05 inspect", err)
	}
	e1 := bytes.Equal(imgFromInit, finalLive)
	shrunk := int64(len(finalLive)) < origStat.Size()/2
	held := repDel.Segment != nil && repVac.Segment != nil && repVac.Snapshot != nil && shrunk && e1 && insp.Integrity == "ok"
	report("SHIP-05", "shrinking database under walship (DELETE 90% + plain VACUUM with SnapshotEvery=1 mirror check)",
		"walship replays shrinking WAL commit, passes periodic mirror check, and PlanFrom(initial) matches closed DB E1",
		fmt.Sprintf("size %d->%d bytes; periodicSnap=%t; integrity=%s; E1(PlanFrom(12)==live)=%t",
			origStat.Size(), len(finalLive), repVac.Snapshot != nil, insp.Integrity, e1), held)
}

// attackSHIP06 tests non-default page sizes (1024 and 8192) under walship and
// confirms ErrNotWAL when opening a rollback-mode configuration.
func attackSHIP06(ctx context.Context, work string) {
	dir := filepath.Join(work, "ship06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship06 mkdir", err)
	}
	defer os.RemoveAll(dir)

	// 1. ErrNotWAL check on rollback-mode config.
	badOpts := recipe.Default().SQLiteOptions()
	badOpts.File.JournalMode = sqlitex.JournalDelete
	badStore, _ := walship.NewDirStore(filepath.Join(dir, "bad-store"))
	_, notWALErr := walship.Open(ctx, walship.Config{Database: filepath.Join(dir, "bad.db"), Store: badStore, Options: badOpts})

	// 2. Full ship + checkpoint + restore on page_size=1024 and page_size=8192.
	pageSizes := []int{1024, 8192}
	psOK := 0
	var psDetail []string
	for _, ps := range pageSizes {
		sub := filepath.Join(dir, fmt.Sprintf("ps-%d", ps))
		dbPath := filepath.Join(sub, "r.db")
		_ = os.MkdirAll(sub, 0o755)
		opts := sqlitex.DefaultOptions()
		opts.File.PageSize = ps
		opts.File.JournalMode = sqlitex.JournalWAL
		opts.File.AutoVacuum = sqlitex.AutoVacuumNone
		opts.Conn.WALAutocheckpoint = 0
		opts.Conn.BusyTimeout = 5 * time.Second
		opts.Conn.SecureDelete = true

		// Initialize schema and seed rows.
		initDB, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("ship06 init open", err)
		}
		if _, err := initDB.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body TEXT); INSERT INTO t VALUES (1, 'seed'); PRAGMA user_version=1;"); err != nil {
			fatal("ship06 init sql", err)
		}
		_ = initDB.Close()

		st, _ := walship.NewDirStore(filepath.Join(sub, "store"))
		sh, err := walship.Open(ctx, walship.Config{Database: dbPath, Store: st, Options: opts, SnapshotEvery: 2, Origin: 1})
		if err != nil {
			psDetail = append(psDetail, fmt.Sprintf("ps=%d openErr=%v", ps, err))
			continue
		}
		ok := true
		for step := 2; step <= 5; step++ {
			wOpts := opts
			wOpts.SingleConnection = true
			wOpts.ImmediateTx = true
			wdb, err := sqlitex.Open(ctx, dbPath, wOpts)
			if err != nil {
				ok = false
				psDetail = append(psDetail, fmt.Sprintf("ps=%d wopen=%v", ps, err))
				break
			}
			tx, err := wdb.BeginTx(ctx, nil)
			if err != nil {
				wdb.Close()
				ok = false
				break
			}
			blob := strings.Repeat(strconv.Itoa(step), ps*2) // forces overflow pages at this page size
			_, e1 := tx.ExecContext(ctx, "INSERT INTO t VALUES (?, ?)", step, blob)
			_, e2 := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", step))
			e3 := tx.Commit()
			_ = wdb.Close()
			if err := errors.Join(e1, e2, e3); err != nil {
				ok = false
				psDetail = append(psDetail, fmt.Sprintf("ps=%d write=%v", ps, err))
				break
			}
			if _, err := sh.Cycle(ctx); err != nil {
				ok = false
				psDetail = append(psDetail, fmt.Sprintf("ps=%d cycle=%v", ps, err))
				break
			}
		}
		_ = sh.Close()
		if !ok {
			continue
		}
		liveBytes, _ := os.ReadFile(dbPath)
		cat, err := walship.LoadCatalog(ctx, st)
		if err != nil {
			psDetail = append(psDetail, fmt.Sprintf("ps=%d cat=%v", ps, err))
			continue
		}
		plan, err := cat.PlanFrom(1, 5)
		if err != nil {
			psDetail = append(psDetail, fmt.Sprintf("ps=%d plan=%v", ps, err))
			continue
		}
		restored, _, err := plan.Execute()
		if err != nil || !bytes.Equal(restored, liveBytes) {
			psDetail = append(psDetail, fmt.Sprintf("ps=%d exec=%v eq=%t", ps, err, bytes.Equal(restored, liveBytes)))
			continue
		}
		psOK++
		psDetail = append(psDetail, fmt.Sprintf("ps=%d E1=ok(%dB)", ps, len(restored)))
	}

	held := errors.Is(notWALErr, walship.ErrNotWAL) && psOK == len(pageSizes)
	report("SHIP-06", "non-default page sizes (1024, 8192 with overflow pages) under walship + ErrNotWAL enforcement",
		"both page sizes ship across checkpoints and restore E1; rollback config returns ErrNotWAL",
		fmt.Sprintf("notWAL=%t; %s", errors.Is(notWALErr, walship.ErrNotWAL), strings.Join(psDetail, "; ")), held)
}

// attackSHIP07 races 8 concurrent goroutines on DirStore.PutIfAbsent across 50 rounds
// and tests WriteDatabase's refusal to overwrite or write next to sidecars.
func attackSHIP07(ctx context.Context, work string) {
	dir := filepath.Join(work, "ship07")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("ship07 mkdir", err)
	}
	defer os.RemoveAll(dir)

	st, err := walship.NewDirStore(filepath.Join(dir, "store"))
	if err != nil {
		fatal("ship07 store", err)
	}

	const rounds = 50
	const racers = 8
	roundsOK := 0
	for round := 0; round < rounds; round++ {
		key := fmt.Sprintf("race/k%04d", round)
		start := make(chan struct{})
		type res struct {
			id  int
			err error
		}
		results := make([]res, racers)
		var wg sync.WaitGroup
		for g := 0; g < racers; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				<-start
				payload := bytes.Repeat([]byte{byte('A' + id)}, 4096)
				results[id] = res{id: id, err: st.PutIfAbsent(ctx, key, payload)}
			}(g)
		}
		close(start)
		wg.Wait()

		winners, existsErrs, winnerID := 0, 0, -1
		for _, r := range results {
			if r.err == nil {
				winners++
				winnerID = r.id
			} else if errors.Is(r.err, walship.ErrExists) {
				existsErrs++
			}
		}
		gotBytes, getErr := st.Get(ctx, key)
		wantBytes := bytes.Repeat([]byte{byte('A' + winnerID)}, 4096)
		if winners == 1 && existsErrs == racers-1 && getErr == nil && bytes.Equal(gotBytes, wantBytes) {
			roundsOK++
		}
	}
	tmpEntries, _ := os.ReadDir(filepath.Join(dir, "store", ".tmp"))

	// WriteDatabase sidecar safety checks.
	wdDir := filepath.Join(dir, "wd")
	_ = os.MkdirAll(wdDir, 0o755)
	sidecarsOK := 0
	for i, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		target := filepath.Join(wdDir, fmt.Sprintf("db%d.sqlite", i))
		_ = os.WriteFile(target+suffix, []byte("x"), 0o644)
		if err := walship.WriteDatabase(target, []byte("data")); errors.Is(err, walship.ErrExists) {
			sidecarsOK++
		}
	}

	held := roundsOK == rounds && len(tmpEntries) == 0 && sidecarsOK == 4
	report("SHIP-07", "8-way concurrent DirStore.PutIfAbsent race (50 rounds) + WriteDatabase sidecar refusal",
		"exactly 1 winner & 7 ErrExists per round, zero torn reads, zero .tmp leftovers, 4/4 sidecar refusals",
		fmt.Sprintf("raceOK=%d/%d tmpLeft=%d writeDBRefusals=%d/4", roundsOK, rounds, len(tmpEntries), sidecarsOK), held)
}

func main() {
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case modeShipOpen:
			err = runShipOpenChild(os.Args[2:])
		case modeShipCycle:
			err = runShipCycleChild(os.Args[2:])
		case modeSpillAndCrash:
			err = runSpillCrashChild(os.Args[2:])
		default:
			if procs.Serve(procs.Roles) {
				return
			}
			goto parent
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

parent:
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	work := filepath.Join(root, "_work", "adv-ch09", "walship-falsify")
	if err := os.RemoveAll(work); err != nil {
		fatal("clean work", err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		fatal("mkdir work", err)
	}

	r := recipe.Default().WithSteps(16)
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}

	attackSHIP01(ctx, r, hashes, work)
	attackSHIP02(ctx, r, hashes, work)
	attackSHIP03(ctx, r, hashes, work)
	attackSHIP04(ctx, self, r, work)
	attackSHIP05(ctx, r, work)
	attackSHIP06(ctx, work)
	attackSHIP07(ctx, work)
}
