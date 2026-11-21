// Experiment 1.2 traces one committed INSERT through every SQLite layer:
// VDBE bytecode (EXPLAIN), B-tree pages (sqlite_dbpage), pager and VFS calls
// (C CLI -vfstrace), and kernel syscalls (strace). It runs in both rollback
// (DELETE) and WAL modes, performs the twin check comparing the translated Go
// driver against the C CLI, and demonstrates the twin check's compile-option
// limit with ANALYZE (ENABLE_STAT4).
//
// Run from the repo root (Linux for strace; on macOS run inside Docker via
// docker compose run --rm lab go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert):
//
//	go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch01-architecture-and-execution-path/experiments/1.2-one-insert"

// On-disk geometry and SQL shared by the rollback and WAL runs: 4096-byte
// pages, 32-byte WAL header, and 24-byte WAL frame header (src/wal.c:21-60).
const (
	pageSize       = 4096
	walHeaderSize  = 32
	walFrameHeader = 24
	createSQL      = "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one')"
	insertSQL      = "INSERT INTO t(id, v) VALUES(2, 'two')"
	dbName         = "t.db"
)

var failures []string

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func options(mode string) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = mode
	o.SingleConnection = true
	return o
}

// cliPrelude translates sqlitex.Options into PRAGMA statements so the C CLI
// runs under the exact same connection settings as the Go driver.
func cliPrelude(o sqlitex.Options) string {
	onOff := func(b bool) string {
		if b {
			return "ON"
		}
		return "OFF"
	}
	c := o.Conn
	return fmt.Sprintf("PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		o.File.JournalMode, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete), c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

func markerPath(dir, name string) string {
	return filepath.Join(dir, "marker-"+name)
}

// createMarkers creates empty files that both the Go child and the CLI (.read)
// open immediately before and after a statement. Because CLI stdout is
// buffered, opening a named file puts an unbuffered syscall bracket in strace
// at the exact statement boundary.
func createMarkers(dir string, names ...string) {
	for _, name := range names {
		if err := os.WriteFile(markerPath(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

func touchMarker(dir, name string) {
	if _, err := os.ReadFile(markerPath(dir, name)); err != nil {
		fatal("marker", err)
	}
}

func createBase(ctx context.Context, path, mode string) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("create base", err)
	}
	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		fatal("create base", err)
	}
	if err := db.Close(); err != nil {
		fatal("close base", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("base at rest", err)
	}
}

// pageHashes reads every page through the sqlite_dbpage virtual table and
// returns its SHA-256 hex digest in page-number order.
func pageHashes(ctx context.Context, path, mode string) []string {
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("open for sqlite_dbpage", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT pgno, data FROM sqlite_dbpage ORDER BY pgno")
	if err != nil {
		fatal("sqlite_dbpage", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var pgno int
		var data []byte
		if err := rows.Scan(&pgno, &data); err != nil {
			fatal("sqlite_dbpage scan", err)
		}
		if pgno != len(hashes)+1 {
			fatal("sqlite_dbpage", fmt.Errorf("page %d out of order", pgno))
		}
		sum := sha256.Sum256(data)
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_dbpage", err)
	}
	return hashes
}

func changedPages(before, after []string) []int {
	var changed []int
	for i := range max(len(before), len(after)) {
		if i >= len(before) || i >= len(after) || before[i] != after[i] {
			changed = append(changed, i+1)
		}
	}
	return changed
}

func short(hash string) string {
	if hash == "" {
		return "(none)"
	}
	return hash[:16]
}

// printExplain queries SQLite's bytecode listing for insertSQL, which
// sqlite3Step produces via sqlite3VdbeList (src/vdbeapi.c:863-865).
func printExplain(ctx context.Context, path string) {
	db, err := sqlitex.Open(ctx, path, options(sqlitex.JournalDelete))
	if err != nil {
		fatal("open for EXPLAIN", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "EXPLAIN "+insertSQL)
	if err != nil {
		fatal("EXPLAIN", err)
	}
	defer rows.Close()
	fmt.Printf("EXPLAIN %s\n", insertSQL)
	fmt.Printf("  %-4s %-14s %4s %4s %4s %-12s %s\n", "addr", "opcode", "p1", "p2", "p3", "p4", "p5")
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			fatal("EXPLAIN scan", err)
		}
		fmt.Printf("  %-4d %-14s %4d %4d %4d %-12s %d\n", addr, opcode, p1, p2, p3, p4.String, p5)
	}
	if err := rows.Err(); err != nil {
		fatal("EXPLAIN", err)
	}
}

type walFrame struct {
	Index      int
	Page       uint32
	CommitSize uint32
}

// readWALFrames decodes the page number (bytes 0..3) and commit-size field
// (bytes 4..7) from every 24-byte frame header after the 32-byte WAL header.
func readWALFrames(path string) ([]walFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var frames []walFrame
	for offset := walHeaderSize; offset+walFrameHeader+pageSize <= len(data); offset += walFrameHeader + pageSize {
		frames = append(frames, walFrame{
			Index:      len(frames) + 1,
			Page:       binary.BigEndian.Uint32(data[offset:]),
			CommitSize: binary.BigEndian.Uint32(data[offset+4:]),
		})
	}
	return frames, nil
}

// childInsert is the re-exec'd child entry point traced by strace. It locks
// its goroutine to one OS thread so strace sees its syscalls in order, brackets
// the INSERT with marker files, and reads the -wal frames before db.Close
// checkpoints and deletes the WAL.
func childInsert(dir, mode string) {
	runtime.LockOSThread()
	ctx := context.Background()
	path := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, path, options(mode))
	if err != nil {
		fatal("child open", err)
	}
	touchMarker(dir, "1")
	if _, err := db.ExecContext(ctx, insertSQL); err != nil {
		fatal("child insert", err)
	}
	touchMarker(dir, "2")
	if mode == sqlitex.JournalWAL {
		// Read the -wal before closing, since closing the last WAL connection
		// checkpoints frames into t.db and deletes t.db-wal. Reading the -wal
		// (not t.db) avoids touching the POSIX locks held on t.db and t.db-shm.
		frames, err := readWALFrames(path + "-wal")
		if err != nil {
			fatal("child read wal", err)
		}
		for _, frame := range frames {
			fmt.Printf("frame %d page=%d commit_size=%d\n", frame.Index, frame.Page, frame.CommitSize)
		}
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func parseChildFrames(stdout string) []walFrame {
	var frames []walFrame
	for _, line := range strings.Split(stdout, "\n") {
		var frame walFrame
		if _, err := fmt.Sscanf(line, "frame %d page=%d commit_size=%d", &frame.Index, &frame.Page, &frame.CommitSize); err == nil {
			frames = append(frames, frame)
		}
	}
	return frames
}

func isMarkerOpen(call trace.Syscall, dir string) (string, bool) {
	if call.Name != "open" && call.Name != "openat" {
		return "", false
	}
	prefix := markerPath(dir, "")
	if !strings.HasPrefix(call.Path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(call.Path, prefix), true
}

func isMarkerFile(call trace.Syscall, dir string) bool {
	return strings.HasPrefix(call.Path, markerPath(dir, ""))
}

var fileSyscalls = []string{"open", "openat", "close", "pread64", "pwrite64", "read", "write", "fsync", "fdatasync", "ftruncate", "unlink", "unlinkat", "fcntl", "mmap"}

func isDatabaseIO(call trace.Syscall, dir string) bool {
	if !slices.Contains(fileSyscalls, call.Name) || isMarkerFile(call, dir) {
		return false
	}
	return call.Path == dir || strings.HasPrefix(call.Path, dir+string(filepath.Separator))
}

// segmentCalls extracts all database-directory file syscalls strictly between
// the open of marker from and the open of marker to.
func segmentCalls(calls []trace.Syscall, dir, from, to string) ([]trace.Syscall, error) {
	start, end := -1, -1
	for i, call := range calls {
		name, ok := isMarkerOpen(call, dir)
		if !ok {
			continue
		}
		if name == from && start < 0 {
			start = i
		}
		if name == to && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("markers %s..%s not found in the trace", from, to)
	}
	var out []trace.Syscall
	for _, call := range calls[start+1 : end] {
		if isDatabaseIO(call, dir) {
			out = append(out, call)
		}
	}
	return trace.RelativeTo(dir, out), nil
}

func syncName(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

// normalized canonicalizes syscall spellings that differ across libc or
// configure builds (open vs openat, unlink vs unlinkat, fsync vs fdatasync)
// while preserving errno on failed calls, short-transfer byte counts, and
// read/write lengths so the twin check compares the actual file operations.
func normalized(call trace.Syscall) string {
	text := normalizedOperation(call)
	switch {
	case call.Errno != "":
		text += " = " + call.Errno
	case transfersBytes(call) && call.Length >= 0 && call.Result != call.Length:
		text += fmt.Sprintf(" = %d", call.Result)
	}
	return text
}

func transfersBytes(call trace.Syscall) bool {
	switch call.Name {
	case "pwrite64", "pread64", "write", "read":
		return true
	}
	return false
}

func normalizedOperation(call trace.Syscall) string {
	switch {
	case call.Name == "open" || call.Name == "openat":
		return "open " + call.Path
	case syncName(call.Name):
		return "sync " + call.Path
	case call.Name == "unlink" || call.Name == "unlinkat":
		return "unlink " + call.Path
	case call.Name == "pwrite64" || call.Name == "pread64":
		return fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
	case call.Name == "write" || call.Name == "read":
		return fmt.Sprintf("%s %s len=%d", call.Name, call.Path, call.Length)
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case call.Name == "mmap":
		return fmt.Sprintf("mmap %s off=%d len=%d", call.Path, call.Offset, call.Length)
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, call.Flags)
	}
	return call.Name + " " + call.Path
}

func normalizedAll(calls []trace.Syscall, keep func(trace.Syscall) bool) []string {
	var out []string
	for _, call := range calls {
		if keep == nil || keep(call) {
			out = append(out, normalized(call))
		}
	}
	return out
}

// isCloexecFcntl matches the extra fcntl(fd, F_SETFD, FD_CLOEXEC) call that
// modernc's musl-derived libc issues after every O_CLOEXEC open, whereas
// SQLite's C build relies on O_CLOEXEC directly.
func isCloexecFcntl(call trace.Syscall) bool {
	return call.Name == "fcntl" && call.Lock == nil && strings.HasPrefix(call.Flags, "F_SETFD")
}

func isWrite(call trace.Syscall) bool {
	return call.Name == "pwrite64" || call.Name == "write" || call.Name == "ftruncate"
}

func isSync(call trace.Syscall) bool {
	return syncName(call.Name)
}

func compareLists(label string, goList, cliList []string) bool {
	if slices.Equal(goList, cliList) {
		fmt.Printf("twin %s: identical (%d calls)\n", label, len(goList))
		return true
	}
	fmt.Printf("twin %s: DIFFERENT (Go %d calls, CLI %d calls)\n", label, len(goList), len(cliList))
	for i := range max(len(goList), len(cliList)) {
		var g, c string
		if i < len(goList) {
			g = goList[i]
		}
		if i < len(cliList) {
			c = cliList[i]
		}
		if g != c {
			fmt.Printf("  #%d Go: %-50s CLI: %s\n", i+1, g, c)
		}
	}
	return false
}

func syncCallNames(calls []trace.Syscall) string {
	var names []string
	for _, call := range calls {
		if isSync(call) && !slices.Contains(names, call.Name) {
			names = append(names, call.Name)
		}
	}
	return strings.Join(names, ",")
}

type phase struct {
	from, to  string
	statement string
	opcode    string
	path      string
}

// phases maps the three split statements (BEGIN IMMEDIATE, INSERT, COMMIT) to
// the opcodes and C call paths they isolate inside one autocommit INSERT.
func phases(mode string) []phase {
	commitPath := "sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne/PhaseTwo -> sqlite3PagerCommitPhaseOne/PhaseTwo"
	insertPath := "sqlite3BtreeInsert -> sqlite3PagerWrite"
	if mode == sqlitex.JournalWAL {
		commitPath = "sqlite3VdbeHalt -> vdbeCommit -> sqlite3BtreeCommitPhaseOne -> sqlite3PagerCommitPhaseOne -> pagerWalFrames -> sqlite3WalFrames"
	}
	return []phase{
		{"1", "2", "BEGIN IMMEDIATE", "OP_Transaction", "sqlite3BtreeBeginTrans -> sqlite3PagerSharedLock, sqlite3PagerBegin"},
		{"2", "3", insertSQL, "OP_Insert", insertPath},
		{"3", "4", "COMMIT", "OP_AutoCommit (OP_Halt in an autocommit INSERT)", commitPath},
	}
}

func splitScript(dir string, o sqlitex.Options) string {
	return cliPrelude(o) +
		".read '" + markerPath(dir, "1") + "'\nBEGIN IMMEDIATE;\n" +
		".read '" + markerPath(dir, "2") + "'\n" + insertSQL + ";\n" +
		".read '" + markerPath(dir, "3") + "'\nCOMMIT;\n" +
		".read '" + markerPath(dir, "4") + "'\n"
}

func autocommitScript(dir string, o sqlitex.Options) string {
	return cliPrelude(o) +
		".read '" + markerPath(dir, "1") + "'\n" + insertSQL + ";\n" +
		".read '" + markerPath(dir, "2") + "'\n"
}

type eventSegment struct {
	events []trace.Event
	calls  []trace.Syscall
}

func mergedSegment(merged *trace.Merged, dir, from, to string) (eventSegment, error) {
	start, end := -1, -1
	for i, event := range merged.Events {
		if event.Kind != trace.EventSyscall {
			continue
		}
		name, ok := isMarkerOpen(*event.Syscall, dir)
		if !ok {
			continue
		}
		if name == from && start < 0 {
			start = i
		}
		if name == to && start >= 0 && end < 0 {
			end = i
		}
	}
	if start < 0 || end < 0 {
		return eventSegment{}, fmt.Errorf("markers %s..%s not found in the merged trace", from, to)
	}
	var segment eventSegment
	for _, event := range merged.Events[start+1 : end] {
		switch event.Kind {
		case trace.EventVFS:
			segment.events = append(segment.events, event)
		case trace.EventSyscall:
			if isDatabaseIO(*event.Syscall, dir) {
				segment.events = append(segment.events, event)
				segment.calls = append(segment.calls, *event.Syscall)
			}
		}
	}
	segment.calls = trace.RelativeTo(dir, segment.calls)
	return segment, nil
}

func vfsLine(call trace.VFSCall, dir string) string {
	calls := trace.RelativeVFS(dir, []trace.VFSCall{call})
	return strings.TrimPrefix(strings.TrimSpace(calls[0].Raw), "trace.")
}

func runMode(ctx context.Context, base, mode string, cli sqlitex.CLI, haveCLI bool) {
	dir := filepath.Join(base, mode)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(dir, dbName)
	createMarkers(dir, "1", "2", "3", "4")
	title := map[string]string{sqlitex.JournalDelete: "rollback journal (journal_mode=delete)", sqlitex.JournalWAL: "write-ahead log (journal_mode=wal, the platform default)"}[mode]
	fmt.Printf("\n== %s ==\n", title)

	createBase(ctx, path, mode)
	before := pageHashes(ctx, path, mode)
	baseHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	fmt.Printf("base: table t with 1 row, %d pages, sha256 %s\n", len(before), short(baseHash))

	// On non-Linux hosts, strace is unavailable; still run the INSERT in
	// process so the reader sees which pages changed.
	if !trace.Supported {
		db, err := sqlitex.Open(ctx, path, options(mode))
		if err != nil {
			fatal("open", err)
		}
		if _, err := db.ExecContext(ctx, insertSQL); err != nil {
			fatal("insert", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		if err := recipe.CheckAtRest(path); err != nil {
			fatal("at rest", err)
		}
		after := pageHashes(ctx, path, mode)
		changed := changedPages(before, after)
		printChanged(before, after, changed)
		checkChangedPages(mode, changed)
		return
	}

	command, err := trace.SelfCommand("child-insert", dir, mode)
	if err != nil {
		fatal("self command", err)
	}
	goRun, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace Go child", err)
	}
	if goRun.ExitCode != 0 {
		fatal("Go child", fmt.Errorf("exit %d: %s", goRun.ExitCode, goRun.Stderr))
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("Go child at rest", err)
	}
	after := pageHashes(ctx, path, mode)
	goFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	changed := changedPages(before, after)
	printChanged(before, after, changed)
	checkChangedPages(mode, changed)

	goCalls, err := segmentCalls(goRun.Log.Calls, dir, "1", "2")
	if err != nil {
		fatal("Go segment", err)
	}
	fmt.Printf("Go driver (modernc), file syscalls of the INSERT (%d):\n", len(goCalls))
	for _, call := range goCalls {
		fmt.Printf("  %s\n", call)
	}

	dbWrites := 0
	offsetsMatch := true
	written := map[int]int{}
	for _, call := range goCalls {
		if call.Name == "pwrite64" && call.Path == dbName {
			dbWrites++
			page := int(call.Offset/pageSize) + 1
			written[page]++
			if call.Offset%pageSize != 0 || call.Length != pageSize || !slices.Contains(changed, page) {
				offsetsMatch = false
			}
			fmt.Printf("  db write at offset %d = (page %d - 1) * %d\n", call.Offset, page, pageSize)
		}
	}
	if mode == sqlitex.JournalDelete {
		// In rollback mode, pager_write_pagelist writes each modified page
		// directly to t.db at offset (page - 1) * pageSize.
		everyChangedWrittenOnce := len(written) == len(changed)
		for _, page := range changed {
			if written[page] != 1 {
				everyChangedWrittenOnce = false
			}
		}
		check(dbWrites > 0 && offsetsMatch && everyChangedWrittenOnce, fmt.Sprintf("rollback mode: each pwrite64 on %s is one whole page at offset (page-1)*4096, and the pages written are exactly the changed pages %v", dbName, changed))
	} else {
		// In WAL mode, pagerWalFrames appends each modified page as a frame in
		// t.db-wal and leaves t.db untouched until a checkpoint runs.
		check(dbWrites == 0, fmt.Sprintf("WAL mode: the commit writes nothing to %s itself (database-file pwrite64 calls during the INSERT: %d)", dbName, dbWrites))
		frames := parseChildFrames(goRun.Stdout)
		var framePages []int
		for _, frame := range frames {
			fmt.Printf("  WAL frame %d: page %d, commit size %d, header at offset %d, page image at offset %d\n", frame.Index, frame.Page, frame.CommitSize, walHeaderSize+(frame.Index-1)*(walFrameHeader+pageSize), walHeaderSize+(frame.Index-1)*(walFrameHeader+pageSize)+walFrameHeader)
			framePages = append(framePages, int(frame.Page))
		}
		var walWrites []string
		for _, call := range goCalls {
			if call.Name == "pwrite64" && call.Path == dbName+"-wal" {
				walWrites = append(walWrites, fmt.Sprintf("%d+%d", call.Offset, call.Length))
			}
		}
		walWritesMatch := slices.Equal(walWrites, expectedFreshWALWrites(frames))
		lastCommit := len(frames) > 0 && frames[len(frames)-1].CommitSize > 0
		check(slices.Equal(framePages, changed) && walWritesMatch && lastCommit, fmt.Sprintf("WAL mode: the commit writes the 32-byte WAL header at offset 0 of the new %s-wal, then one frame per changed page %v (frame header 24 bytes, then the page at offset 32 + (frame-1)*4120 + 24), and nothing else; the last frame carries the commit size", dbName, changed))
	}

	if !haveCLI {
		return
	}

	// Twin check: rebuild the base and run the exact same autocommit INSERT in
	// the C CLI under strace, then compare writes, syncs, other syscalls, and
	// final file bytes.
	createBase(ctx, path, mode)
	cliCommand := cli.Command(ctx, sqlitex.CLIRun{}).Args
	cliCommand[0] = cli.Path
	cliRun, err := trace.Run(ctx, trace.Options{Command: cliCommand, Dir: dir, Stdin: strings.NewReader(sqlitex.OpenScript(path, autocommitScript(dir, options(mode))))})
	if err != nil {
		fatal("strace CLI", err)
	}
	if cliRun.ExitCode != 0 {
		fatal("CLI", fmt.Errorf("exit %d: %s", cliRun.ExitCode, cliRun.Stderr))
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("CLI at rest", err)
	}
	cliFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	cliCalls, err := segmentCalls(cliRun.Log.Calls, dir, "1", "2")
	if err != nil {
		fatal("CLI segment", err)
	}
	fmt.Printf("C CLI, file syscalls of the same INSERT (%d, open/openat shown as open, fsync/fdatasync as sync):\n", len(cliCalls))
	for _, line := range normalizedAll(cliCalls, nil) {
		fmt.Printf("  %s\n", line)
	}
	fmt.Printf("INFO: sync syscall used: Go driver %s, CLI %s (%s)\n", syncCallNames(goCalls), syncCallNames(cliCalls), displayPath(cli.Path))
	writesSame := compareLists("writes (file, offset, length, order)", normalizedAll(goCalls, isWrite), normalizedAll(cliCalls, isWrite))
	syncsSame := compareLists("syncs (file, order)", normalizedAll(goCalls, isSync), normalizedAll(cliCalls, isSync))
	goSyncs := normalizedAll(goCalls, isSync)
	check(writesSame && syncsSame && len(goSyncs) > 0, "twin check: the Go driver and the C CLI make the same writes (file, offset, length) and the same syncs, in the same order")
	goCloexec := normalizedAll(goCalls, isCloexecFcntl)
	cliCloexec := normalizedAll(cliCalls, isCloexecFcntl)
	fmt.Printf("twin difference: fcntl F_SETFD calls, Go driver %d [%s], CLI %d [%s]\n", len(goCloexec), strings.Join(goCloexec, "; "), len(cliCloexec), strings.Join(cliCloexec, "; "))
	notCloexec := func(call trace.Syscall) bool { return !isCloexecFcntl(call) }
	allSame := compareLists("every other file syscall (opens, reads, writes, syncs, locks, unlinks, closes)", normalizedAll(goCalls, notCloexec), normalizedAll(cliCalls, notCloexec))
	check(allSame, "apart from fcntl F_SETFD, every file syscall of the INSERT is the same in both builds (open vs openat and fsync vs fdatasync are not distinguished)")
	check(goFileHash == cliFileHash && goFileHash != baseHash, fmt.Sprintf("the database file after the INSERT is byte-identical from both builds (sha256 %s)", short(goFileHash)))

	// Measured call path: split the INSERT into BEGIN IMMEDIATE, INSERT, and
	// COMMIT under CLI -vfstrace + strace so each VFS call and syscall is
	// attributed to the opcode that caused it, then verify the combined
	// syscalls equal the autocommit INSERT's.
	createBase(ctx, path, mode)
	merged, splitRun, err := trace.RunCLIMerged(ctx, cli, trace.CLITrace{Dir: dir, DB: path, SQL: splitScript(dir, options(mode))}, trace.Options{})
	if err != nil {
		fatal("CLI -vfstrace under strace", err)
	}
	if splitRun.ExitCode != 0 {
		fatal("CLI -vfstrace", fmt.Errorf("exit %d: %s", splitRun.ExitCode, splitRun.Stderr))
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("CLI -vfstrace at rest", err)
	}
	splitFileHash, err := recipe.FileSHA256(path)
	if err != nil {
		fatal("hash", err)
	}
	fmt.Println("call path, measured: the C CLI with -vfstrace under strace runs BEGIN IMMEDIATE; INSERT; COMMIT with a marker between statements.")
	fmt.Println("Each phase lists the VFS calls (VFS) and the syscalls each one made (SYS, indented under its VFS call).")
	var concatenated []trace.Syscall
	for _, p := range phases(mode) {
		segment, err := mergedSegment(merged, dir, p.from, p.to)
		if err != nil {
			fatal("merged segment", err)
		}
		concatenated = append(concatenated, segment.calls...)
		fmt.Printf("phase %s | opcode %s | %s\n", p.statement, p.opcode, p.path)
		vfsCount := 0
		for _, event := range segment.events {
			switch event.Kind {
			case trace.EventVFS:
				if event.VFS.Method == "xRandomness" {
					// Different CLI builds seed SQLite's PRNG at different
					// points, so keep xRandomness on an INFO line.
					fmt.Printf("INFO: phase %s: VFS %s (SQLite seeds its PRNG on first use; this CLI build first needs randomness here)\n", p.statement, vfsLine(*event.VFS, dir))
					continue
				}
				vfsCount++
				fmt.Printf("  VFS %s\n", vfsLine(*event.VFS, dir))
			case trace.EventSyscall:
				relative := trace.RelativeTo(dir, []trace.Syscall{*event.Syscall})[0]
				indent := "  "
				if event.Enclosed >= 0 {
					indent = "      "
				}
				fmt.Printf("%sSYS %s\n", indent, normalized(relative))
			}
		}
		if vfsCount == 0 {
			fmt.Println("  (no VFS calls)")
		}
	}
	check(slices.Equal(normalizedAll(concatenated, nil), normalizedAll(cliCalls, nil)), "the three phases together make exactly the file syscalls of the autocommit INSERT (so the per-opcode split above is the INSERT's own I/O)")
	check(splitFileHash == cliFileHash, "BEGIN IMMEDIATE; INSERT; COMMIT leaves the same file bytes as the autocommit INSERT")
}

// expectedChangedPages returns the 1-based pages that the INSERT modifies:
// pages 1 and 2 in rollback mode (header change counter plus table leaf), and
// page 2 alone in WAL mode (page 1 is not dirtied).
func expectedChangedPages(mode string) []int {
	if mode == sqlitex.JournalWAL {
		return []int{2}
	}
	return []int{1, 2}
}

// checkChangedPages verifies that sqlite_dbpage observed exactly the pages
// expected for mode (runs on both Linux and non-Linux hosts).
func checkChangedPages(mode string, changed []int) {
	want := expectedChangedPages(mode)
	reason := "page 1 (the change counter in the header) and page 2 (table t's leaf)"
	if mode == sqlitex.JournalWAL {
		reason = "only page 2 (table t's leaf); page 1 is not written, so its change counter stays"
	}
	check(slices.Equal(changed, want), fmt.Sprintf("the INSERT changes %s: %v", reason, want))
}

// expectedFreshWALWrites returns the complete sequence of "offset+length"
// pwrite64 operations for a freshly created WAL file: the 32-byte WAL header
// at offset 0 followed by each frame's 24-byte header and 4096-byte page image.
func expectedFreshWALWrites(frames []walFrame) []string {
	if len(frames) == 0 {
		return nil
	}
	want := []string{fmt.Sprintf("0+%d", walHeaderSize)}
	for _, frame := range frames {
		offset := walHeaderSize + (frame.Index-1)*(walFrameHeader+pageSize)
		want = append(want, fmt.Sprintf("%d+%d", offset, walFrameHeader), fmt.Sprintf("%d+%d", offset+walFrameHeader, pageSize))
	}
	return want
}

func printChanged(before, after []string, changed []int) {
	fmt.Printf("pages changed by the INSERT (sha256 of each page through sqlite_dbpage, before -> after): %v\n", changed)
	for _, page := range changed {
		var b, a string
		if page <= len(before) {
			b = before[page-1]
		}
		if page <= len(after) {
			a = after[page-1]
		}
		fmt.Printf("  page %d: %s -> %s\n", page, short(b), short(a))
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "child-insert" {
		childInsert(os.Args[2], os.Args[3])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("1.2")
	base, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(base))
	var cli sqlitex.CLI
	haveCLI := false
	if trace.Supported {
		located, cliErr := sqlitex.LocateCLI(ctx)
		cli, haveCLI = located, cliErr == nil
		if haveCLI {
			fmt.Println(cli.PinLine())
		} else {
			fmt.Printf("sqlite3 CLI: unavailable: %v\n", cliErr)
			failures = append(failures, "the pinned sqlite3 CLI is available")
		}
	}

	explainPath := filepath.Join(base, "explain.db")
	createBase(ctx, explainPath, sqlitex.JournalDelete)
	printExplain(ctx, explainPath)

	if !trace.Supported {
		trace.PrintLinuxOnly("the strace of the Go driver and of the CLI, the twin check, its ANALYZE limit and the measured call path", packagePath)
	}
	for _, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		runMode(ctx, base, mode, cli, haveCLI)
	}
	if haveCLI {
		twinLimitAnalyze(ctx, base, cli)
	}

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("ONE INSERT FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("ONE INSERT OK")
}

const (
	analyzeSchemaSQL = "CREATE TABLE s(id INTEGER PRIMARY KEY, k INTEGER NOT NULL); CREATE INDEX s_by_k ON s(k); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) INSERT INTO s(id, k) SELECT i, i % 7 FROM n"
	stat4OptionSQL   = "SELECT sqlite_compileoption_used('ENABLE_STAT4')"
)

// statTables returns the sorted names of any sqlite_stat* tables created by
// ANALYZE in the database at path.
func statTables(ctx context.Context, path string) []string {
	db, err := sqlitex.Open(ctx, path, options(sqlitex.JournalWAL))
	if err != nil {
		fatal("open for sqlite_schema", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE name LIKE 'sqlite_stat%' ORDER BY name")
	if err != nil {
		fatal("sqlite_schema", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			fatal("sqlite_schema scan", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		fatal("sqlite_schema", err)
	}
	return names
}

// twinLimitAnalyze demonstrates a boundary of the twin check: starting from
// byte-identical copies of an indexed table, ANALYZE writes sqlite_stat4 in
// the ENABLE_STAT4-enabled Go driver but only sqlite_stat1 in the CLI, so the
// resulting database files diverge.
func twinLimitAnalyze(ctx context.Context, base string, cli sqlitex.CLI) {
	fmt.Println("\n== a limit of the twin check: compile options that change file contents ==")
	dir := filepath.Join(base, "analyze")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	goPath := filepath.Join(dir, "go.db")
	cliPath := filepath.Join(dir, "cli.db")
	o := options(sqlitex.JournalWAL)
	for _, path := range []string{goPath, cliPath} {
		if err := recipe.RemoveDatabase(path); err != nil {
			fatal("remove", err)
		}
		db, err := sqlitex.Open(ctx, path, o)
		if err != nil {
			fatal("create analyze base", err)
		}
		if _, err := db.ExecContext(ctx, analyzeSchemaSQL); err != nil {
			fatal("create analyze base", err)
		}
		if err := db.Close(); err != nil {
			fatal("close analyze base", err)
		}
	}
	baseGo, err := recipe.FileSHA256(goPath)
	if err != nil {
		fatal("hash", err)
	}
	baseCLI, err := recipe.FileSHA256(cliPath)
	if err != nil {
		fatal("hash", err)
	}
	fmt.Printf("base: table s with 100 rows and index s_by_k, built twice by the Go driver, sha256 %s and %s\n", short(baseGo), short(baseCLI))

	db, err := sqlitex.Open(ctx, goPath, o)
	if err != nil {
		fatal("open", err)
	}
	var goStat4 int
	if err := db.QueryRowContext(ctx, stat4OptionSQL).Scan(&goStat4); err != nil {
		fatal("compile option", err)
	}
	if _, err := db.ExecContext(ctx, "ANALYZE"); err != nil {
		fatal("ANALYZE", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	out, err := cli.Exec(ctx, cliPath, cliPrelude(o)+stat4OptionSQL+";\nANALYZE;\n")
	if err != nil {
		fatal("CLI ANALYZE", err)
	}
	lines := strings.Fields(out)
	cliStat4 := -1
	if len(lines) > 0 {
		fmt.Sscan(lines[len(lines)-1], &cliStat4)
	}
	for _, path := range []string{goPath, cliPath} {
		if err := recipe.CheckAtRest(path); err != nil {
			fatal("analyze at rest", err)
		}
	}
	goHash, err := recipe.FileSHA256(goPath)
	if err != nil {
		fatal("hash", err)
	}
	cliHash, err := recipe.FileSHA256(cliPath)
	if err != nil {
		fatal("hash", err)
	}
	goTables, cliTables := statTables(ctx, goPath), statTables(ctx, cliPath)
	fmt.Printf("ENABLE_STAT4 compiled in: Go driver %d, CLI %d\n", goStat4, cliStat4)
	fmt.Printf("after ANALYZE: Go driver file sha256 %s, statistics tables %v\n", short(goHash), goTables)
	fmt.Printf("after ANALYZE: C CLI file sha256 %s, statistics tables %v\n", short(cliHash), cliTables)
	check(baseGo == baseCLI, "both copies start byte-identical")
	if goStat4 == 1 && cliStat4 == 0 {
		check(goHash != cliHash && slices.Contains(goTables, "sqlite_stat4") && !slices.Contains(cliTables, "sqlite_stat4"),
			"twin check limit: the driver is compiled with ENABLE_STAT4 and this CLI is not, so the same ANALYZE writes a sqlite_stat4 table only in the driver and the two files differ")
	} else {
		check(goStat4 == cliStat4 && goHash == cliHash, "with the same ENABLE_STAT4 setting the same ANALYZE leaves byte-identical files")
	}
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
