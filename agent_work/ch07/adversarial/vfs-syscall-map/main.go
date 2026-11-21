// Group 1 adversarial attacks for Chapter 7, Experiment 7.1 (vfs-calls-to-syscalls).
// Attacks:
//
//	MAP-01: Go driver vs C CLI syscall twin check on 7.1's exact statement sequence.
//	MAP-02: SIGKILL leaves -wal and -shm behind; next open recovers row and cleans up on close.
//	MAP-03a: vfstrace prints FULL for real SQLITE_SYNC_NORMAL (0x02) at NORMAL and FULL.
//	MAP-03b: PRAGMA fullfsync=ON yields real SQLITE_SYNC_FULL (0x03) and FULL|DATAONLY (0x13).
//	MAP-04: PRAGMA mmap_size bypasses xRead/pread64 via xFetch (boundary of 7.1's read claim).
//	MAP-05: xAccess(EXISTS) calls stat (not access) and treats a 0-byte journal as absent.
//	MAP-06: synchronous=EXTRA adds a second directory sync inside xDelete (dirSync=1).
//	MAP-07: Must-be-able-to-fail check on 7.1's VFS-to-syscall attribution.
//	MAP-08: Verify 7.1's macOS portable() check matches actual CLI -vfstrace xShmUnmap syntax.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/govfs"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
	_ "modernc.org/sqlite"
)

const (
	scratchRoot = "_work/adv-ch07/vfs-syscall-map"
	dbName      = "t.db"
)

var tracedSyscalls = append(
	append([]string(nil), trace.DefaultSyscalls...),
	"fstat", "newfstatat", "statx", "stat", "lstat", "access", "faccessat", "faccessat2",
)

var exp71Statements = []string{
	"PRAGMA journal_mode=WAL;",
	"PRAGMA synchronous=FULL;",
	"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL);",
	"INSERT INTO t(id, v) VALUES(1, 'one');",
	"PRAGMA wal_checkpoint(TRUNCATE);",
}

func emit(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

func cleanDir(path string) string {
	if err := os.RemoveAll(path); err != nil {
		fatal("remove "+path, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		fatal("mkdir "+path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fatal("abs "+path, err)
	}
	return abs
}

func fileSHA256(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("read "+path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func listDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fatal("readdir "+dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func isDBArtifact(path string) bool {
	return path == "." || path == dbName || strings.HasPrefix(path, dbName+"-")
}

// runChildGo71 executes 7.1's exact statement sequence in-process using modernc.org/sqlite.
func runChildGo71(dbPath string) {
	runtime.LockOSThread()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		fatal("child go open", err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range exp71Statements {
		if strings.HasPrefix(stmt, "PRAGMA ") {
			rows, err := db.Query(stmt)
			if err != nil {
				fatal("child go "+stmt, err)
			}
			for rows.Next() {
			}
			if err := rows.Close(); err != nil {
				fatal("child go close rows", err)
			}
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			fatal("child go "+stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("child go close", err)
	}
}

// runChildKillWAL opens a WAL DB, commits one row, prints READY, and waits to be SIGKILLed.
func runChildKillWAL(dbPath string) {
	ctx := context.Background()
	opts := sqlitex.DefaultOptions()
	opts.File.JournalMode = sqlitex.JournalWAL
	opts.Conn.Synchronous = sqlitex.SyncFull
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		fatal("child kill open", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
		fatal("child kill create", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(1, 'alive')"); err != nil {
		fatal("child kill insert", err)
	}
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = db.Close()
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "child-go-71":
			runChildGo71(os.Args[2])
			return
		case "child-kill-wal":
			runChildKillWAL(os.Args[2])
			return
		}
	}
	if !trace.Supported {
		fatal("platform", trace.ErrLinuxOnly)
	}
	ctx := context.Background()
	cli := sqlitex.MustLocateCLI(ctx)
	v, err := govfs.Register("adv-map-govfs", govfs.Options{})
	if err != nil {
		fatal("register govfs", err)
	}

	attackMAP01(ctx, cli)
	attackMAP02(ctx, cli)
	attackMAP03(ctx, cli, v)
	attackMAP04(ctx, cli)
	attackMAP05(ctx, cli)
	attackMAP06(ctx, cli)
	attackMAP07(ctx, cli)
	attackMAP08(ctx, cli)
}

type ioSummary struct {
	writes    []string
	syncs     []string
	truncates []string
	unlinks   []string
	dirSyncs  int
}

func summarizeSyscalls(calls []trace.Syscall) ioSummary {
	var s ioSummary
	for _, c := range calls {
		if !isDBArtifact(c.Path) {
			continue
		}
		name := trace.CanonicalName(c.Name)
		switch name {
		case "pwrite64":
			s.writes = append(s.writes, fmt.Sprintf("%s@%d+%d", c.Path, c.Offset, c.Length))
		case "sync":
			s.syncs = append(s.syncs, c.Path)
			if c.Path == "." {
				s.dirSyncs++
			}
		case "ftruncate":
			s.truncates = append(s.truncates, fmt.Sprintf("%s=%d", c.Path, c.Length))
		case "unlink":
			s.unlinks = append(s.unlinks, c.Path)
		}
	}
	return s
}

func firstDiff(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return fmt.Sprintf("idx %d: go=%q cli=%q", i, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return fmt.Sprintf("len go=%d cli=%d", len(a), len(b))
	}
	return "equal"
}

// MAP-01: Run 7.1's exact statement sequence through Go driver vs C CLI under strace.
func attackMAP01(ctx context.Context, cli sqlitex.CLI) {
	goDir := cleanDir(filepath.Join(scratchRoot, "map01-go"))
	cliDir := cleanDir(filepath.Join(scratchRoot, "map01-cli"))
	goDB := filepath.Join(goDir, dbName)
	cliDB := filepath.Join(cliDir, dbName)

	cmd, err := trace.SelfCommand("child-go-71", goDB)
	if err != nil {
		fatal("self command", err)
	}
	goRes, err := trace.Run(ctx, trace.Options{Command: cmd, Dir: goDir, Syscalls: tracedSyscalls})
	if err != nil {
		fatal("map01 go strace", err)
	}
	if goRes.ExitCode != 0 {
		fatal("map01 go child", fmt.Errorf("exit %d: %s", goRes.ExitCode, goRes.Stderr))
	}

	script := strings.Join(exp71Statements, "\n") + "\n"
	_, cliRes, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: cliDir, DB: cliDB, SQL: script}, trace.Options{Syscalls: tracedSyscalls})
	if err != nil {
		fatal("map01 cli strace", err)
	}
	if cliRes.ExitCode != 0 {
		fatal("map01 cli", fmt.Errorf("exit %d: %s", cliRes.ExitCode, cliRes.Stderr))
	}

	goSum := summarizeSyscalls(trace.RelativeTo(goDir, goRes.Log.Calls))
	cliSum := summarizeSyscalls(trace.RelativeTo(cliDir, cliRes.Log.Calls))
	goFiles := listDirNames(goDir)
	cliFiles := listDirNames(cliDir)
	goHash := fileSHA256(goDB)
	cliHash := fileSHA256(cliDB)

	writesEq := slices.Equal(goSum.writes, cliSum.writes)
	syncsEq := slices.Equal(goSum.syncs, cliSum.syncs)
	truncEq := slices.Equal(goSum.truncates, cliSum.truncates)
	unlinkEq := slices.Equal(goSum.unlinks, cliSum.unlinks)
	filesEq := slices.Equal(goFiles, []string{dbName}) && slices.Equal(cliFiles, []string{dbName})
	hashEq := goHash == cliHash

	held := writesEq && syncsEq && truncEq && unlinkEq && filesEq && hashEq
	got := fmt.Sprintf("writes(%d vs %d:%s) syncs(%d vs %d dir=%d/%d:%s) ftruncate(%s) unlink(%s) files=%v/%v sha256=%s/%s",
		len(goSum.writes), len(cliSum.writes), firstDiff(goSum.writes, cliSum.writes),
		len(goSum.syncs), len(cliSum.syncs), goSum.dirSyncs, cliSum.dirSyncs, firstDiff(goSum.syncs, cliSum.syncs),
		firstDiff(goSum.truncates, cliSum.truncates),
		firstDiff(goSum.unlinks, cliSum.unlinks),
		goFiles, cliFiles, goHash, cliHash)
	emit("MAP-01",
		"7.1 statement sequence in Go driver vs C CLI under strace (twin check)",
		"identical pwrite64/sync/ftruncate/unlink sequences, 2 dir syncs, only t.db left, equal sha256",
		got, held)
}

// MAP-02: SIGKILL leaves -wal and -shm on disk; next open recovers row and cleans up on close.
func attackMAP02(ctx context.Context, cli sqlitex.CLI) {
	dir := cleanDir(filepath.Join(scratchRoot, "map02-kill"))
	dbPath := filepath.Join(dir, dbName)

	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	child := exec.CommandContext(ctx, self, "child-kill-wal", dbPath)
	stdin, err := child.StdinPipe()
	if err != nil {
		fatal("stdin pipe", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		fatal("stdout pipe", err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fatal("start child-kill-wal", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "READY" {
		_ = child.Process.Kill()
		fatal("child-kill-wal ready", fmt.Errorf("got %q err=%v", line, err))
	}
	if err := child.Process.Kill(); err != nil {
		fatal("sigkill", err)
	}
	_ = stdin.Close()
	_ = child.Wait()

	afterKill := listDirNames(dir)
	hasWAL := slices.Contains(afterKill, dbName+"-wal")
	hasSHM := slices.Contains(afterKill, dbName+"-shm")

	merged, res, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{
		Dir: dir,
		DB:  dbPath,
		SQL: "SELECT id, v FROM t;\nPRAGMA integrity_check;\n",
	}, trace.Options{Syscalls: tracedSyscalls})
	if err != nil {
		fatal("map02 reopen", err)
	}
	if res.ExitCode != 0 {
		fatal("map02 cli", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	rel := merged.Relative(dir)
	var outputs []string
	walDeleted, shmUnmapped := false, false
	for _, e := range rel.Events {
		switch e.Kind {
		case trace.EventOutput:
			outputs = append(outputs, e.Output)
		case trace.EventVFS:
			if e.VFS.Method == "xDelete" && e.VFS.File == dbName+"-wal" {
				walDeleted = true
			}
			if e.VFS.Method == "xShmUnmap" && strings.Contains(e.VFS.Raw, "delFlag=1") {
				shmUnmapped = true
			}
		}
	}
	afterClose := listDirNames(dir)
	rowOK := slices.Contains(outputs, "1|alive") && slices.Contains(outputs, "ok")
	cleanClose := walDeleted && shmUnmapped && slices.Equal(afterClose, []string{dbName})

	held := hasWAL && hasSHM && rowOK && cleanClose
	emit("MAP-02",
		"SIGKILL live WAL writer, then reopen and close via CLI under -vfstrace+strace",
		"-wal and -shm survive SIGKILL; next open recovers row 1|alive + integrity ok and unlinks both on close",
		fmt.Sprintf("afterKill=%v outputs=%v xDelete(wal)=%t xShmUnmap(del=1)=%t afterClose=%v", afterKill, outputs, walDeleted, shmUnmapped, afterClose),
		held)
}

// MAP-03a and MAP-03b: Verify vfstrace FULL-for-NORMAL quirk and PRAGMA fullfsync=ON flags.
func attackMAP03(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS) {
	type probe struct {
		label     string
		journal   string
		sync      string
		fullfsync bool
	}
	runPair := func(p probe) (realFlags []string, vfsLabels []string, linuxSyncs int) {
		dir := cleanDir(filepath.Join(scratchRoot, "map03-"+p.label))
		goPath := filepath.Join(dir, "go.db")
		cliPath := filepath.Join(dir, "cli.db")

		o := sqlitex.DefaultOptions()
		o.File.JournalMode = p.journal
		o.Conn.Synchronous = p.sync
		o.SingleConnection = true
		o.VFS = v.Name()
		db, err := sqlitex.Open(ctx, goPath, o)
		if err != nil {
			fatal("map03 go open", err)
		}
		if p.fullfsync {
			if _, err := db.ExecContext(ctx, "PRAGMA fullfsync=ON"); err != nil {
				fatal("map03 fullfsync", err)
			}
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1,'a')"); err != nil {
			fatal("map03 setup", err)
		}
		v.Reset()
		if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES(2,'b')"); err != nil {
			fatal("map03 insert", err)
		}
		for _, c := range v.TakeCalls() {
			if c.Method == "xSync" {
				realFlags = append(realFlags, fmt.Sprintf("0x%x(%s)", c.Flags, govfs.SyncFlagsString(c.Flags)))
			}
		}
		_ = db.Close()

		ff := "OFF"
		if p.fullfsync {
			ff = "ON"
		}
		script := fmt.Sprintf("PRAGMA page_size=4096;\nPRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA fullfsync=%s;\nCREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT);\nINSERT INTO t VALUES(1,'a');\n.print MARK-BEGIN\nINSERT INTO t VALUES(2,'b');\n.print MARK-END\n",
			p.journal, p.sync, ff)
		merged, res, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: dir, DB: cliPath, SQL: script}, trace.Options{Syscalls: tracedSyscalls})
		if err != nil {
			fatal("map03 cli", err)
		}
		if res.ExitCode != 0 {
			fatal("map03 cli exit", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
		}
		inside := false
		for _, e := range merged.Events {
			if e.Kind == trace.EventOutput {
				if e.Output == "MARK-BEGIN" {
					inside = true
				} else if e.Output == "MARK-END" {
					inside = false
				}
				continue
			}
			if !inside {
				continue
			}
			if e.Kind == trace.EventVFS && e.VFS.Method == "xSync" && len(e.VFS.Args) >= 2 {
				vfsLabels = append(vfsLabels, e.VFS.Args[1])
			}
			if e.Kind == trace.EventSyscall && trace.IsSync(*e.Syscall) {
				linuxSyncs++
			}
		}
		return realFlags, vfsLabels, linuxSyncs
	}

	// MAP-03a: default fullfsync=OFF across delete/NORMAL, delete/FULL, wal/FULL
	dnReal, dnCLI, _ := runPair(probe{"del-norm", sqlitex.JournalDelete, sqlitex.SyncNormal, false})
	dfReal, dfCLI, _ := runPair(probe{"del-full", sqlitex.JournalDelete, sqlitex.SyncFull, false})
	wfReal, wfCLI, _ := runPair(probe{"wal-full", sqlitex.JournalWAL, sqlitex.SyncFull, false})

	allNormalReal := slices.Equal(dnReal, []string{"0x2(NORMAL)", "0x2(NORMAL)"}) &&
		slices.Equal(dfReal, []string{"0x2(NORMAL)", "0x2(NORMAL)", "0x2(NORMAL)"}) &&
		slices.Equal(wfReal, []string{"0x2(NORMAL)"})
	allFullPrinted := slices.Equal(dnCLI, []string{"FULL", "FULL"}) &&
		slices.Equal(dfCLI, []string{"FULL", "FULL", "FULL"}) &&
		slices.Equal(wfCLI, []string{"FULL"})

	emit("MAP-03a",
		"compare govfs real xSync flags vs CLI -vfstrace labels at delete/NORMAL, delete/FULL, wal/FULL (fullfsync=OFF)",
		"real flags are all 0x2(NORMAL) while vfstrace prints FULL for every sync (vfstrace.c:453 bitmask quirk)",
		fmt.Sprintf("del/NORM real=%v cli=%v; del/FULL real=%v cli=%v; wal/FULL real=%v cli=%v", dnReal, dnCLI, dfReal, dfCLI, wfReal, wfCLI),
		allNormalReal && allFullPrinted)

	// MAP-03b: PRAGMA fullfsync=ON in delete/FULL should pass 0x3(FULL), 0x13(FULL|DATAONLY), 0x3(FULL)
	ffReal, ffCLI, ffSys := runPair(probe{"del-full-ff", sqlitex.JournalDelete, sqlitex.SyncFull, true})
	wantReal := []string{"0x3(FULL)", "0x13(FULL|DATAONLY)", "0x3(FULL)"}
	wantCLI := []string{"FULL", "FULL|DATAONLY", "FULL"}
	emit("MAP-03b",
		"delete/FULL with PRAGMA fullfsync=ON through govfs and CLI -vfstrace",
		"real xSync flags become [0x3(FULL), 0x13(FULL|DATAONLY), 0x3(FULL)] and vfstrace prints [FULL, FULL|DATAONLY, FULL]",
		fmt.Sprintf("real=%v cli=%v linuxSyncSyscalls=%d", ffReal, ffCLI, ffSys),
		slices.Equal(ffReal, wantReal) && slices.Equal(ffCLI, wantCLI))
}

// MAP-04: PRAGMA mmap_size bypasses xRead/pread64 for data pages via xFetch.
func attackMAP04(ctx context.Context, cli sqlitex.CLI) {
	dir := cleanDir(filepath.Join(scratchRoot, "map04-mmap"))
	dbPath := filepath.Join(dir, dbName)

	// Build a multi-page DB first.
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = sqlitex.JournalWAL
	o.SingleConnection = true
	db, err := sqlitex.Open(ctx, dbPath, o)
	if err != nil {
		fatal("map04 build", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL)"); err != nil {
		fatal("map04 create", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("map04 begin", err)
	}
	payload := strings.Repeat("m", 600)
	for i := 1; i <= 100; i++ {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t VALUES(?, ?)", i, payload); err != nil {
			fatal("map04 insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("map04 commit", err)
	}
	_ = db.Close()

	// Run a fresh CLI session with mmap_size=268435456 and trace SELECT scan.
	script := "PRAGMA mmap_size=268435456;\n.print SCAN-BEGIN\nSELECT count(*), sum(length(v)) FROM t;\n.print SCAN-END\n"
	merged, res, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: dir, DB: dbPath, SQL: script}, trace.Options{Syscalls: tracedSyscalls})
	if err != nil {
		fatal("map04 cli", err)
	}
	if res.ExitCode != 0 {
		fatal("map04 cli exit", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	inside := false
	xReads, xReadsWithoutPread, xFetches, preads := 0, 0, 0, 0
	var scanOut string
	rel := merged.Relative(dir)
	for i, e := range rel.Events {
		if e.Kind == trace.EventOutput {
			switch e.Output {
			case "SCAN-BEGIN":
				inside = true
			case "SCAN-END":
				inside = false
			default:
				if inside {
					scanOut = e.Output
				}
			}
			continue
		}
		if !inside {
			continue
		}
		if e.Kind == trace.EventVFS {
			switch e.VFS.Method {
			case "xRead":
				xReads++
				hasPread := false
				for _, s := range rel.SyscallsWithin(i) {
					if trace.CanonicalName(s.Name) == "pread64" && s.Path == dbName {
						hasPread = true
					}
				}
				if !hasPread {
					xReadsWithoutPread++
				}
			case "xFetch":
				xFetches++
			}
		}
		if e.Kind == trace.EventSyscall && trace.CanonicalName(e.Syscall.Name) == "pread64" && e.Syscall.Path == dbName {
			preads++
		}
	}
	// 7.1's 1-to-1 xRead -> pread64 mapping holds with mmap off (the default, MMAP_SIZE=0).
	// With PRAGMA mmap_size>0, reads still cross the VFS via xFetch (pager.c:5687) and mapped
	// unixRead memcpy (os_unix.c:3533-3548), bypassing pread64.
	claimHeld := scanOut == "100|60000" && xFetches > 0 && xReadsWithoutPread > 0
	emit("MAP-04",
		"full table scan (100x600B rows) in CLI -vfstrace+strace with PRAGMA mmap_size=268435456",
		"with mmap_size>0, data reads pass through xFetch and mapped unixRead (0 pread64), narrowing 7.1's xRead->pread64 rule to default mmap_size=0",
		fmt.Sprintf("scan=%q xRead=%d xReadWith0Pread=%d pread64=%d xFetch=%d", scanOut, xReads, xReadsWithoutPread, preads, xFetches),
		claimHeld)
}

// MAP-05: xAccess(EXISTS) calls stat (never access) and treats a 0-byte journal as absent.
func attackMAP05(ctx context.Context, cli sqlitex.CLI) {
	probeJournal := func(sub string, content []byte) (xAccessOut string, statCalls, accessCalls int) {
		dir := cleanDir(filepath.Join(scratchRoot, "map05-"+sub))
		dbPath := filepath.Join(dir, dbName)
		o := sqlitex.DefaultOptions()
		o.File.JournalMode = sqlitex.JournalDelete
		o.SingleConnection = true
		db, err := sqlitex.Open(ctx, dbPath, o)
		if err != nil {
			fatal("map05 open", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY); INSERT INTO t VALUES(1)"); err != nil {
			fatal("map05 setup", err)
		}
		_ = db.Close()

		if err := os.WriteFile(dbPath+"-journal", content, 0o644); err != nil {
			fatal("map05 write journal", err)
		}
		merged, res, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{
			Dir: dir,
			DB:  dbPath,
			SQL: "SELECT count(*) FROM t;\n",
		}, trace.Options{Syscalls: tracedSyscalls})
		if err != nil {
			fatal("map05 cli", err)
		}
		if res.ExitCode != 0 {
			fatal("map05 cli exit", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
		}
		rel := merged.Relative(dir)
		for i, e := range rel.Events {
			if e.Kind == trace.EventVFS && e.VFS.Method == "xAccess" && e.VFS.File == dbName+"-journal" {
				xAccessOut = e.VFS.Raw
				for _, s := range rel.SyscallsWithin(i) {
					switch trace.CanonicalName(s.Name) {
					case "stat":
						statCalls++
					case "access":
						accessCalls++
					}
				}
				break
			}
		}
		return xAccessOut, statCalls, accessCalls
	}

	out0, stat0, acc0 := probeJournal("empty", nil)
	out1, stat1, acc1 := probeJournal("nonempty", make([]byte, 512))

	held := strings.Contains(out0, "out=0") && stat0 >= 1 && acc0 == 0 &&
		strings.Contains(out1, "out=1") && stat1 >= 1 && acc1 == 0
	emit("MAP-05",
		"xAccess(EXISTS) on 0-byte vs 512-byte t.db-journal under CLI -vfstrace+strace",
		"xAccess uses stat (0 access() syscalls), reports out=0 for 0-byte file and out=1 for 512-byte file",
		fmt.Sprintf("0-byte:(%s stat=%d access=%d) 512-byte:(%s stat=%d access=%d)", out0, stat0, acc0, out1, stat1, acc1),
		held)
}

// MAP-06: synchronous=EXTRA adds a directory sync inside xDelete (syncDir=1).
func attackMAP06(ctx context.Context, cli sqlitex.CLI) {
	dir := cleanDir(filepath.Join(scratchRoot, "map06-extra"))
	dbPath := filepath.Join(dir, dbName)
	script := "PRAGMA page_size=4096;\nPRAGMA journal_mode=DELETE;\nPRAGMA synchronous=EXTRA;\nCREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT);\nINSERT INTO t VALUES(1,'a');\n.print MARK-BEGIN\nINSERT INTO t VALUES(2,'b');\n.print MARK-END\n"
	merged, res, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: dir, DB: dbPath, SQL: script}, trace.Options{Syscalls: tracedSyscalls})
	if err != nil {
		fatal("map06 cli", err)
	}
	if res.ExitCode != 0 {
		fatal("map06 cli exit", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	rel := merged.Relative(dir)
	inside := false
	dirSyncInSync, dirSyncInDelete := 0, 0
	totalSyncs := 0
	deleteDirSyncArg := ""
	for i, e := range rel.Events {
		if e.Kind == trace.EventOutput {
			if e.Output == "MARK-BEGIN" {
				inside = true
			} else if e.Output == "MARK-END" {
				inside = false
			}
			continue
		}
		if !inside {
			continue
		}
		if e.Kind == trace.EventSyscall && trace.IsSync(*e.Syscall) && isDBArtifact(e.Syscall.Path) {
			totalSyncs++
		}
		if e.Kind == trace.EventVFS && e.VFS.Method == "xSync" {
			for _, s := range rel.SyscallsWithin(i) {
				if trace.IsSync(s) && s.Path == "." {
					dirSyncInSync++
				}
			}
		}
		if e.Kind == trace.EventVFS && e.VFS.Method == "xDelete" && e.VFS.File == dbName+"-journal" {
			if len(e.VFS.Args) >= 2 {
				deleteDirSyncArg = e.VFS.Args[1]
			}
			for _, s := range rel.SyscallsWithin(i) {
				if trace.IsSync(s) && s.Path == "." {
					dirSyncInDelete++
				}
			}
		}
	}
	held := totalSyncs == 5 && dirSyncInSync == 1 && dirSyncInDelete == 1 && deleteDirSyncArg == "1"
	emit("MAP-06",
		"rollback DELETE commit at synchronous=EXTRA under CLI -vfstrace+strace",
		"5 sync syscalls per commit (2 journal + 1 db + 1 dir in xSync + 1 dir in xDelete with dirSync=1)",
		fmt.Sprintf("totalSyncs=%d dirSyncInSync=%d dirSyncInDelete=%d xDelete.dirSync=%q", totalSyncs, dirSyncInSync, dirSyncInDelete, deleteDirSyncArg),
		held)
}

// MAP-07: Must-be-able-to-fail check: run 7.1 with a wrapper CLI that tampers with one xWrite offset
// in vfstrace output and verify 7.1 fails loudly (exits non-zero with "check FAILED").
func attackMAP07(ctx context.Context, cli sqlitex.CLI) {
	dir := cleanDir(filepath.Join(scratchRoot, "map07-fail"))
	wrapper := filepath.Join(dir, "fake-sqlite3.sh")
	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("map07 mkdir", err)
	}
	// Pass pin/version queries untouched; when -vfstrace is passed, rewrite ofst=0 to ofst=999999
	// on stdout so 7.1's VFS-to-syscall matcher must detect the mismatch and fail.
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
for arg in "$@"; do
  if [ "$arg" = "-vfstrace" ]; then
    exec "%s" "$@" | sed -u 's/ofst=0)/ofst=999999)/'
  fi
done
exec "%s" "$@"
`, cli.Path, cli.Path)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		fatal("map07 write wrapper", err)
	}
	cmd := exec.CommandContext(ctx, "go", "run", "./ch07-vfs-and-filesystem-io/experiments/7.1-vfs-calls-to-syscalls")
	cmd.Env = append(os.Environ(),
		"SQLITE3_BIN="+wrapper,
		"COURSE_WORK_DIR="+workDir,
	)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			fatal("map07 run 7.1", err)
		}
	}
	text := string(out)
	sawFailedCheck := strings.Contains(text, "check FAILED:")
	held := exitCode != 0 && sawFailedCheck
	var firstFail string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "check FAILED:") {
			firstFail = line
			break
		}
	}
	emit("MAP-07",
		"must-be-able-to-fail: run 7.1 with SQLITE3_BIN mutating xWrite ofst=0 -> 999999",
		"7.1 exits non-zero and prints check FAILED on xWrite/pwrite64 mismatch",
		fmt.Sprintf("exit=%d firstFail=%q", exitCode, firstFail),
		held)
}

// MAP-08: Verify 7.1's repaired macOS portable() check (closeLifecycle matching delFlag=1 in c.Args)
// against actual CLI -vfstrace output and 7.1's source.
func attackMAP08(ctx context.Context, cli sqlitex.CLI) {
	dir := cleanDir(filepath.Join(scratchRoot, "map08-portable"))
	dbPath := filepath.Join(dir, dbName)
	vt, err := trace.RunCLIVFSTrace(ctx, cli, trace.CLITrace{
		Dir: dir,
		DB:  dbPath,
		SQL: strings.Join(exp71Statements, "\n") + "\n",
	})
	if err != nil {
		fatal("map08 vfstrace", err)
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("map08 repo root", err)
	}
	srcBytes, err := os.ReadFile(filepath.Join(root, "ch07-vfs-and-filesystem-io", "experiments", "7.1-vfs-calls-to-syscalls", "main.go"))
	if err != nil {
		fatal("map08 read 7.1", err)
	}
	src := string(srcBytes)
	srcUsesDelFlag := strings.Contains(src, `"delFlag=1"`) && !strings.Contains(src, `"delete=1"`)
	var unmapRaw string
	matchedDelFlagArg := false
	for _, c := range trace.RelativeVFS(dir, vt.Calls) {
		if c.Method == "xShmUnmap" {
			unmapRaw = c.Raw
			if slices.Contains(c.Args, "delFlag=1") {
				matchedDelFlagArg = true
			}
		}
	}
	emit("MAP-08",
		"replay 7.1 closeLifecycle() macOS check (delFlag=1 in c.Args) on real CLI -vfstrace",
		"7.1 source checks delFlag=1 (not delete=1) and matches real CLI xShmUnmap close line",
		fmt.Sprintf("srcUsesDelFlag=%t matched_delFlag=1:%t raw=%q", srcUsesDelFlag, matchedDelFlagArg, unmapRaw),
		srcUsesDelFlag && matchedDelFlagArg)
}
