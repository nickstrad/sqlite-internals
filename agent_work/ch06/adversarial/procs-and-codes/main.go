package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const roleSleeper = "sleeper"

// sleeperRole is a minimal test role used to verify internal/procs loud-failure guarantees.
func sleeperRole(ctx context.Context, args []string) (*procs.Actor, error) {
	return &procs.Actor{
		Actions: map[string]procs.Handler{
			"ping": func(ctx context.Context, a []string) (string, error) {
				return "pong", nil
			},
			"sleep": func(ctx context.Context, a []string) (string, error) {
				d := 10 * time.Second
				if len(a) == 1 {
					parsed, err := time.ParseDuration(a[0])
					if err != nil {
						return "", err
					}
					d = parsed
				}
				time.Sleep(d)
				return "woke", nil
			},
		},
	}, nil
}

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

// attackProcs01 verifies internal/procs loud-failure guarantees:
// SIGKILL -> ErrChildDied; hung child -> ErrTimeout + process reaped;
// concurrent Start -> ErrBusyChild; Stop while busy -> ErrBusyChild;
// duplicate Spawn name -> error; use after Stop -> ErrStopped.
func attackProcs01() {
	// 1. SIGKILL mid-request -> ErrChildDied
	c1, err := procs.StartChild("kill-target", roleSleeper)
	if err != nil {
		fatal("procs01 start c1", err)
	}
	call1, err := c1.Start(1, "sleep", "5s")
	if err != nil {
		fatal("procs01 start sleep", err)
	}
	proc, err := os.FindProcess(c1.Pid)
	if err != nil {
		fatal("procs01 find proc", err)
	}
	if err := proc.Kill(); err != nil {
		fatal("procs01 kill", err)
	}
	_, errKill := call1.Wait()
	gotDied := errors.Is(errKill, procs.ErrChildDied)

	// 2. Timeout -> ErrTimeout and child process is actually gone
	c2, err := procs.StartChild("timeout-target", roleSleeper)
	if err != nil {
		fatal("procs01 start c2", err)
	}
	c2.Timeout = 150 * time.Millisecond
	pid2 := c2.Pid
	_, errTimeout := c2.Do(1, "sleep", "5s")
	gotTimeout := errors.Is(errTimeout, procs.ErrTimeout)
	p2, _ := os.FindProcess(pid2)
	// Signal(0) returns ESRCH once the child has been killed and reaped.
	childGone := p2.Signal(syscall.Signal(0)) != nil

	// 3. Concurrent Start -> ErrBusyChild, and Stop while busy -> ErrBusyChild
	c3, err := procs.StartChild("busy-target", roleSleeper)
	if err != nil {
		fatal("procs01 start c3", err)
	}
	if _, err := c3.Start(1, "sleep", "5s"); err != nil {
		fatal("procs01 c3 start", err)
	}
	_, errBusyStart := c3.Start(2, "ping")
	gotBusyStart := errors.Is(errBusyStart, procs.ErrBusyChild)
	errBusyStop := c3.Stop()
	gotBusyStop := errors.Is(errBusyStop, procs.ErrBusyChild)

	// 4. Use after Stop -> ErrStopped; duplicate Spawn name -> error
	script := procs.NewScript(io.Discard)
	c4, err := script.Spawn("dup", roleSleeper)
	if err != nil {
		fatal("procs01 spawn dup", err)
	}
	_, errDup := script.Spawn("dup", roleSleeper)
	gotDupErr := errDup != nil
	if err := script.Close(); err != nil {
		fatal("procs01 close script", err)
	}
	_, errAfterStop := c4.Start(1, "ping")
	gotStopped := errors.Is(errAfterStop, procs.ErrStopped)

	held := gotDied && gotTimeout && childGone && gotBusyStart && gotBusyStop && gotDupErr && gotStopped
	report(
		"PROCS-01",
		"must-be-able-to-fail internal/procs invariants: SIGKILL, timeout+reap, concurrent Start, Stop while busy, duplicate name, use after Stop",
		"every violation fails loudly with its typed sentinel error and leaves no live orphan child",
		fmt.Sprintf("sigkill_died=%v timeout=%v reaped=%v busy_start=%v busy_stop=%v dup_err=%v stopped=%v",
			gotDied, gotTimeout, childGone, gotBusyStart, gotBusyStop, gotDupErr, gotStopped),
		held,
	)
}

// attackProcs02a parses the live host's /proc/locks via procs.ReadProcLocks() (Linux only)
// to test whether any real kernel lock line breaks ParseProcLockLine.
func attackProcs02a() {
	if !procs.ProcLocksSupported {
		report(
			"PROCS-02a",
			"parse live /proc/locks via procs.ReadProcLocks()",
			"non-linux returns ErrNoProcLocks",
			"skipped on "+runtime.GOOS,
			true,
		)
		return
	}
	locks, err := procs.ReadProcLocks()
	if err != nil {
		report(
			"PROCS-02a",
			"parse live host /proc/locks via procs.ReadProcLocks()",
			"parses every active kernel lock line without ErrProcLocksFormat",
			fmt.Sprintf("error=%v", err),
			false,
		)
		return
	}
	// Also verify synthetic real-world kernel lock line variants (OFDLCK with pid -1, blocked ->, EOF).
	synthetic := strings.Join([]string{
		"1: POSIX  ADVISORY  WRITE 12345 08:01:9999 120 121",
		"2: -> POSIX  ADVISORY  WRITE 12346 08:01:9999 120 120",
		"3: OFDLCK ADVISORY  READ  -1 08:01:9999 0 EOF",
		"4: FLOCK  ADVISORY  WRITE 2222 00:1a:42 0 EOF",
		"5: LEASE  ACTIVE    READ  3333 08:01:77 0 EOF",
	}, "\n")
	parsed, perr := procs.ParseProcLocks(synthetic)
	held := perr == nil && len(parsed) == 5 && parsed[1].Blocked && parsed[2].Pid == -1 && parsed[2].ToEOF
	report(
		"PROCS-02a",
		"parse live host /proc/locks plus OFDLCK(-1), blocked(->), FLOCK, LEASE, and EOF entries",
		"live /proc/locks and all 5 kernel lock line variants parse cleanly",
		fmt.Sprintf("live_entries=%d synthetic_parsed=%d err=%v", len(locks), len(parsed), perr),
		held,
	)
}

// attackProcs02b tests DescribeProcLocks on merged & EOF ranges and probes FilterProcLocks'
// inode-only matching (no device check, documented in build-notes).
func attackProcs02b() {
	ranges := procs.ShmLockRanges()
	names := map[int]string{100: "holder"}

	// Merged adjacent locks (bytes 120-121: WRITE+CKPT) and whole-file EOF lock (0-EOF).
	merged := procs.ProcLock{ID: 1, Class: "POSIX", Mode: "ADVISORY", Type: "WRITE", Pid: 100, DevMajor: 8, DevMinor: 1, Inode: 555, Start: 120, End: 121}
	eofLock := procs.ProcLock{ID: 2, Class: "POSIX", Mode: "ADVISORY", Type: "READ", Pid: 100, DevMajor: 8, DevMinor: 1, Inode: 555, Start: 0, End: 1<<62 - 1, ToEOF: true}
	desc := procs.DescribeProcLocks([]procs.ProcLock{merged, eofLock}, names, ranges)
	mergedOK := len(desc) == 2 && strings.Contains(desc[1], "(WAL_WRITE_LOCK+WAL_CKPT_LOCK)")
	eofOK := len(desc) == 2 && strings.Contains(desc[0], "0-EOF") && strings.Contains(desc[0], "WAL_WRITE_LOCK") && strings.Contains(desc[0], "DMS")

	// Cross-device inode collision test: two locks with SAME inode & pid on DIFFERENT devices.
	devA := procs.ProcLock{ID: 10, Class: "POSIX", Mode: "ADVISORY", Type: "WRITE", Pid: 100, DevMajor: 8, DevMinor: 1, Inode: 999, Start: 120, End: 120}
	devB := procs.ProcLock{ID: 11, Class: "POSIX", Mode: "ADVISORY", Type: "WRITE", Pid: 100, DevMajor: 0, DevMinor: 42, Inode: 999, Start: 120, End: 120}
	filtered := procs.FilterProcLocks([]procs.ProcLock{devA, devB}, 999, map[int]bool{100: true})

	// build-notes explicitly states FilterProcLocks matches by inode+pid (not device), so
	// returning both matches its documented behavior; we verify merged+EOF formatting and record len=2.
	held := mergedOK && eofOK && len(filtered) == 2
	report(
		"PROCS-02b",
		"DescribeProcLocks on merged (120-121) and 0-EOF locks; FilterProcLocks cross-device inode collision check",
		"merged prints (WAL_WRITE_LOCK+WAL_CKPT_LOCK), 0-EOF covers all named ranges, FilterProcLocks matches inode+pid ignoring dev (returns 2)",
		fmt.Sprintf("merged_ok=%v eof_ok=%v cross_dev_filtered=%d desc=%q", mergedOK, eofOK, len(filtered), strings.Join(desc, "; ")),
		held,
	)
}

// attackProcs03 verifies fcntl(F_GETLK) observation boundaries:
// 1) Self-process blind spot: F_GETLK from the lock-holding process itself returns Held=false.
// 2) Multi-reader blind spot: F_GETLK sees 1 holder on SHARED while /proc/locks sees both children.
// 3) Invalid byte ranges (start < 0, len <= 0) return errors.
func attackProcs03(ctx context.Context, root string) {
	// 1. Self-process F_GETLK blind spot
	lockFile := filepath.Join(root, "self-lock.dat")
	if err := os.WriteFile(lockFile, make([]byte, 256), 0o644); err != nil {
		fatal("procs03 write", err)
	}
	f, err := os.OpenFile(lockFile, os.O_RDWR, 0)
	if err != nil {
		fatal("procs03 open", err)
	}
	flock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 120, Len: 1}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &flock); err != nil {
		f.Close()
		fatal("procs03 setlk", err)
	}
	// Calling ProbeLock from the SAME process: POSIX F_GETLK never conflicts with own locks,
	// AND ProbeLock's defer file.Close() ALSO drops our own lock!
	selfHolder, err := procs.ProbeLock(lockFile, 120, 1)
	f.Close()
	if err != nil {
		fatal("procs03 self probe", err)
	}
	selfBlind := !selfHolder.Held

	// 2. Invalid ranges rejected
	_, errNegStart := procs.ProbeLock(lockFile, -1, 1)
	_, errZeroLen := procs.ProbeLock(lockFile, 0, 0)
	_, errNegLen := procs.ProbeLock(lockFile, 0, -5)
	invalidRejected := errNegStart != nil && errZeroLen != nil && errNegLen != nil

	// 3. Multi-reader: two children hold SHARED read locks on rollback DB
	db := filepath.Join(root, "multi-reader", "lab.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		fatal("procs03 mkdir", err)
	}
	if err := procs.CreateDatabase(ctx, db, sqlitex.JournalDelete, "CREATE TABLE lab(id INTEGER PRIMARY KEY)"); err != nil {
		fatal("procs03 create", err)
	}
	script := procs.NewScript(io.Discard)
	defer script.Close()
	opts := procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete}
	r1, err := script.Spawn("r1", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("procs03 spawn r1", err)
	}
	r2, err := script.Spawn("r2", procs.RoleSQL, opts.Args()...)
	if err != nil {
		fatal("procs03 spawn r2", err)
	}
	_, _ = script.Do(r1, "begin", "deferred")
	_, _ = script.Do(r1, "query", "SELECT count(*) FROM lab")
	_, _ = script.Do(r2, "begin", "deferred")
	_, _ = script.Do(r2, "query", "SELECT count(*) FROM lab")

	states, err := procs.ProbeRanges(db, procs.DatabaseLockRanges())
	if err != nil {
		fatal("procs03 probe", err)
	}
	getlkRead := procs.ReadLocked(states, "SHARED")
	procCount := -1
	if procs.ProcLocksSupported {
		ino, err := procs.FileInode(db)
		if err != nil {
			fatal("procs03 inode", err)
		}
		all, err := procs.ReadProcLocks()
		if err != nil {
			fatal("procs03 proclocks", err)
		}
		matched := procs.FilterProcLocks(all, ino, map[int]bool{r1.Pid: true, r2.Pid: true})
		procCount = len(matched)
	}
	_ = script.Close()

	multiOK := getlkRead && (!procs.ProcLocksSupported || procCount == 2)
	held := selfBlind && invalidRejected && multiOK
	report(
		"PROCS-03",
		"ProbeLock (F_GETLK): self-process blind spot, invalid ranges, and two concurrent SHARED readers vs /proc/locks",
		"self-process F_GETLK reports Held=false, invalid ranges return error, F_GETLK sees read-locked while /proc/locks lists both readers (2)",
		fmt.Sprintf("self_blind=%v invalid_rejected=%v getlk_read=%v proclocks_readers=%d", selfBlind, invalidRejected, getlkRead, procCount),
		held,
	)
}

// attackProcs04 verifies sqlitex.ResultCodeName constants/fallbacks and sqlitex.DisplayPath
// including symlinks pointing outside the repo.
func attackProcs04(repoRoot, root string) {
	codeChecks := map[int]string{
		sqlitex.CodeOK:                "SQLITE_OK",
		sqlitex.CodeBusy:              "SQLITE_BUSY",
		sqlitex.CodeBusyRecovery:      "SQLITE_BUSY_RECOVERY",
		sqlitex.CodeBusySnapshot:      "SQLITE_BUSY_SNAPSHOT",
		sqlitex.CodeProtocol:          "SQLITE_PROTOCOL",
		sqlitex.CodeIOErrShmLock:      "SQLITE_IOERR_SHMLOCK",
		sqlitex.CodeIOErrDeleteNoEnt:  "SQLITE_IOERR_DELETE_NOENT",
		sqlitex.CodeLockedSharedCache: "SQLITE_LOCKED_SHAREDCACHE",
		sqlitex.CodeBusy | 9<<8:       "SQLITE_BUSY+extended(9)",
		99:                            "SQLITE_code_99",
		-1:                            "SQLITE_code_-1",
	}
	codesOK := true
	var codeBad string
	for code, want := range codeChecks {
		if got := sqlitex.ResultCodeName(code); got != want {
			codesOK = false
			codeBad = fmt.Sprintf("code %d: got %q want %q", code, got, want)
			break
		}
	}
	// Verify numeric values match sqlite.h.in
	valOK := sqlitex.CodeBusySnapshot == 517 &&
		sqlitex.CodeBusyRecovery == 261 &&
		sqlitex.CodeProtocol == 15 &&
		sqlitex.CodeIOErrDeleteNoEnt == 5898 &&
		sqlitex.CodeIOErrShmLock == 5130 &&
		sqlitex.CodeLockedSharedCache == 262

	// DisplayPath checks: inside repo, repo root, outside repo, and symlink pointing outside repo.
	inside := sqlitex.DisplayPath(filepath.Join(repoRoot, "_work", "adv-ch06", "x.db"))
	atRoot := sqlitex.DisplayPath(repoRoot)
	outside := sqlitex.DisplayPath("/tmp/nonexistent-adv-outside.db")

	// Create a symlink inside _work/adv-ch06 pointing to a real file in /tmp
	extFile := filepath.Join(os.TempDir(), fmt.Sprintf("adv-ch06-ext-%d.db", os.Getpid()))
	_ = os.WriteFile(extFile, []byte("x"), 0o644)
	defer os.Remove(extFile)
	linkPath := filepath.Join(root, "escape-link.db")
	_ = os.Remove(linkPath)
	if err := os.Symlink(extFile, linkPath); err != nil {
		fatal("procs04 symlink", err)
	}
	viaLink := sqlitex.DisplayPath(linkPath)
	// Because DisplayPath resolves symlinks via EvalSymlinks, a symlink escaping the repo
	// must NOT return a "../..." path — it must fall back to the base name!
	displayOK := inside == "_work/adv-ch06/x.db" &&
		atRoot == "." &&
		outside == "nonexistent-adv-outside.db" &&
		!strings.HasPrefix(viaLink, "..") &&
		!strings.Contains(viaLink, "/")

	held := codesOK && valOK && displayOK
	report(
		"PROCS-04",
		"sqlitex.ResultCodeName extended codes/fallbacks and sqlitex.DisplayPath (inside, root, outside, escaping symlink)",
		"all ch06 extended codes match sqlite.h.in and DisplayPath never leaks machine paths or '../' escapes",
		fmt.Sprintf("codes_ok=%v(%s) vals_ok=%v display=(in=%q root=%q out=%q link=%q)", codesOK, codeBad, valOK, inside, atRoot, outside, viaLink),
		held,
	)
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func copyTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

// attackProcs05 is a must-be-able-to-fail check on OUTLINE §1 rule 4:
// mutate sqlite/PIN in a scratch copy under _work/adv-ch06/ and verify all four ch06
// experiments (6.1, 6.2, 6.3, 6.4) refuse to run (exit non-zero) on the mismatched pin.
func attackProcs05(ctx context.Context, repoRoot, root string) {
	scratch := filepath.Join(root, "scratch-pin-repo")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("procs05 clean", err)
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if err := copyFile(filepath.Join(repoRoot, f), filepath.Join(scratch, f)); err != nil {
			fatal("procs05 copy "+f, err)
		}
	}
	for _, d := range []string{"sqlite", "internal", "ch06-locking-and-concurrency/experiments"} {
		if err := copyTree(filepath.Join(repoRoot, d), filepath.Join(scratch, d)); err != nil {
			fatal("procs05 copy "+d, err)
		}
	}
	// Mutate fossil_id and source_id in the scratch sqlite/PIN (never touching the real repo).
	pinPath := filepath.Join(scratch, "sqlite", "PIN")
	pinBytes, err := os.ReadFile(pinPath)
	if err != nil {
		fatal("procs05 read pin", err)
	}
	mutated := strings.ReplaceAll(string(pinBytes), "bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc", "0000000000000000000000000000000000000000000000000000000000000000")
	if err := os.WriteFile(pinPath, []byte(mutated), 0o644); err != nil {
		fatal("procs05 write pin", err)
	}

	exps := []string{
		"./ch06-locking-and-concurrency/experiments/6.1-locks-belong-to-the-process",
		"./ch06-locking-and-concurrency/experiments/6.2-busy-rules",
		"./ch06-locking-and-concurrency/experiments/6.3-torn-copy",
		"./ch06-locking-and-concurrency/experiments/6.4-live-snapshot",
	}
	allRefused := true
	var summary []string
	for _, exp := range exps {
		cmd := exec.CommandContext(ctx, "go", "run", exp)
		cmd.Dir = scratch
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		out := buf.String()
		refused := err != nil && (strings.Contains(out, "pin") || strings.Contains(out, "sqlite_source_id"))
		if !refused {
			allRefused = false
		}
		short := filepath.Base(exp)
		summary = append(summary, fmt.Sprintf("%s(refused=%v)", short[:3], refused))
	}
	report(
		"PROCS-05",
		"must-be-able-to-fail: run all 4 ch06 experiments in a scratch repo with mutated sqlite/PIN",
		"all four experiments exit non-zero and report sqlite_source_id pin mismatch",
		strings.Join(summary, " "),
		allRefused,
	)
}

// attackProcs06 checks InspectDatabase and CopierRole safety refusals.
func attackProcs06(ctx context.Context, root string) {
	dir := filepath.Join(root, "safety")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("procs06 mkdir", err)
	}

	// 1. InspectDatabase must refuse a DB with a sibling -wal file.
	dbWal := filepath.Join(dir, "withwal.db")
	if err := procs.CreateDatabase(ctx, dbWal, sqlitex.JournalWAL, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		fatal("procs06 create", err)
	}
	if err := os.WriteFile(dbWal+"-wal", []byte("stale"), 0o644); err != nil {
		fatal("procs06 write wal", err)
	}
	_, errWal := procs.InspectDatabase(ctx, dbWal, sqlitex.JournalWAL, "SELECT 1")

	// 2. InspectDatabase on garbage non-SQLite bytes must return OpenError (NOTADB 26), not crash.
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("NOT A SQLITE DATABASE!", 200)), 0o644); err != nil {
		fatal("procs06 write garbage", err)
	}
	inspGarbage, errGarbage := procs.InspectDatabase(ctx, garbage, sqlitex.JournalDelete, "SELECT 1")
	garbageHandled := errGarbage == nil && strings.Contains(inspGarbage.OpenError, "SQLITE_NOTADB (26)")

	// 3. CopierRole must refuse path-traversal targets.
	script := procs.NewScript(io.Discard)
	defer script.Close()
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", garbage, "-out", dir)
	if err != nil {
		fatal("procs06 spawn copier", err)
	}
	badTargets := []string{"../escape.db", "sub/out.db", ".", "..", ""}
	allRefused := true
	for _, tgt := range badTargets {
		res, err := script.Do(copier, "copy", "db", tgt)
		if err != nil || res.OK() {
			allRefused = false
		}
	}
	_ = script.Close()

	held := errWal != nil && garbageHandled && allRefused
	report(
		"PROCS-06",
		"InspectDatabase refuses live -wal and handles garbage file (NOTADB 26); CopierRole rejects path traversal targets",
		"with-wal returns error, garbage reports OpenError SQLITE_NOTADB (26), all 5 traversal targets rejected",
		fmt.Sprintf("wal_refused=%v garbage_open_err=%q traversal_refused=%v", errWal != nil, inspGarbage.OpenError, allRefused),
		held,
	)
}

// attackProcs02c tests ParseProcLockLine against valid Linux fs/locks.c formats
// that can appear on a reader's host from unrelated processes: <none>:0, MSNFS, UNKNOWN.
func attackProcs02c() {
	lines := []struct {
		name string
		line string
	}{
		{"none-inode", "10: FLOCK  ADVISORY  WRITE 4321 <none>:0 0 EOF"},
		{"msnfs-mode", "11: FLOCK  MSNFS     WRITE 4322 08:01:1234 0 EOF"},
		{"unknown-class", "12: UNKNOWN ADVISORY READ  4323 08:01:1235 0 EOF"},
	}
	okCount := 0
	var failed []string
	for _, tc := range lines {
		if _, err := procs.ParseProcLockLine(tc.line); err != nil {
			failed = append(failed, tc.name)
		} else {
			okCount++
		}
	}
	held := okCount == len(lines)
	report(
		"PROCS-02c",
		"ParseProcLockLine on valid Linux fs/locks.c host variants (<none>:0, MSNFS, UNKNOWN)",
		"ReadProcLocks/ParseProcLockLine tolerates valid unrelated host /proc/locks entries without aborting 6.1",
		fmt.Sprintf("parsed=%d/%d rejected=[%s]", okCount, len(lines), strings.Join(failed, ",")),
		held,
	)
}

func main() {
	roles := map[string]procs.Role{
		procs.RoleSQL:    procs.SQLRole,
		procs.RoleCopier: procs.CopierRole,
		roleSleeper:      sleeperRole,
	}
	if procs.Serve(roles) {
		return
	}
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	workRoot := filepath.Join(repoRoot, "_work", "adv-ch06", "procs-and-codes")
	if err := os.RemoveAll(workRoot); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}

	attackProcs01()
	attackProcs02a()
	attackProcs02b()
	attackProcs02c()
	attackProcs03(ctx, workRoot)
	attackProcs04(repoRoot, workRoot)
	attackProcs05(ctx, repoRoot, workRoot)
	attackProcs06(ctx, workRoot)
}
