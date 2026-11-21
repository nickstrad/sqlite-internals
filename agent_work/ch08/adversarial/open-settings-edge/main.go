// Group 1 (open-settings-edge): adversarial attacks on Experiment 8.1
// (open-cost-and-where-settings-live), sqlitex.SettingPlacements, and ch08
// pin-check enforcement.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
	_ "modernc.org/sqlite"
)

const (
	pageSize   = 4096
	markPrefix = "MARK "
)

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(2)
}

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func openPlain(ctx context.Context, path string, query url.Values) (*sql.DB, *sql.Conn, error) {
	uri, err := sqlitex.FileURI(path, query)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

func closePlain(db *sql.DB, conn *sql.Conn) {
	if conn != nil {
		conn.Close()
	}
	if db != nil {
		db.Close()
	}
}

func readPragma(ctx context.Context, conn *sql.Conn, pragma string) (string, error) {
	var v any
	if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
		return "", err
	}
	if b, ok := v.([]byte); ok {
		return strings.ToLower(string(b)), nil
	}
	return strings.ToLower(fmt.Sprint(v)), nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func buildScratchRepo(repoRoot, scratchRepo string) error {
	if err := os.RemoveAll(scratchRepo); err != nil {
		return err
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if err := copyFile(filepath.Join(repoRoot, f), filepath.Join(scratchRepo, f)); err != nil {
			return err
		}
	}
	dirs := []string{
		"sqlite",
		"internal/sqlitex",
		"internal/trace",
		"internal/recipe",
		"internal/dbfile",
		"internal/equal",
		"internal/procstat",
		"internal/connmgr",
		"ch08-connection-and-database-lifecycle/experiments",
	}
	for _, d := range dirs {
		if err := copyTree(filepath.Join(repoRoot, d), filepath.Join(scratchRepo, d)); err != nil {
			return err
		}
	}
	return nil
}

type cmdResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runGoPkg(ctx context.Context, dir, pkg string, extraEnv []string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, "go", "run", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return cmdResult{}, err
		}
	}
	return cmdResult{exitCode: code, stdout: stdout.String(), stderr: stderr.String()}, nil
}

func firstLine(s string) string {
	s = strings.TrimLeft(s, "\r\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], "\r")
	}
	return strings.TrimRight(s, "\r")
}

// Child roles for OPN-03 strace checks and dirty-WAL setup.
func childTraceOpen(path string) {
	runtime.LockOSThread()
	ctx := context.Background()
	fmt.Println(markPrefix + "open")
	db, conn, err := openPlain(ctx, path, nil)
	if err != nil {
		fatal("child open", err)
	}
	fmt.Println(markPrefix + "first statement")
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&count); err != nil {
		fatal("child query", err)
	}
	fmt.Println(markPrefix + "close")
	closePlain(db, conn)
	fmt.Println(markPrefix + "done")
}

func childDirtyWriter(path string) {
	runtime.LockOSThread()
	ctx := context.Background()
	opts := sqlitex.DefaultOptions()
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("dirty writer open", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (2, 'uncheckpointed-frame')"); err != nil {
		fatal("dirty writer insert", err)
	}
	fmt.Println("ready")
	// Block until parent sends SIGKILL so WAL stays uncheckpointed.
	io.Copy(io.Discard, os.Stdin)
	db.Close()
}

type phaseSummary struct {
	opens      []string
	dbReads    []trace.Syscall
	otherReads map[string]int
}

func parsePhases(log *trace.StraceLog, dir string) (map[string]*phaseSummary, error) {
	out := map[string]*phaseSummary{}
	cur := ""
	under := trace.UnderDir(dir)
	for _, call := range log.Calls {
		if call.Name == "write" && call.FD == 1 && strings.HasPrefix(call.Data, markPrefix) {
			cur = strings.TrimSuffix(strings.TrimPrefix(call.Data, markPrefix), "\n")
			if cur != "done" {
				out[cur] = &phaseSummary{otherReads: map[string]int{}}
			}
			continue
		}
		p, ok := out[cur]
		if !ok || cur == "done" || !under(call) || call.Result < 0 {
			continue
		}
		base := filepath.Base(call.Path)
		switch call.Name {
		case "open", "openat":
			p.opens = append(p.opens, base)
		case "pread64", "read":
			if strings.HasSuffix(base, ".db") {
				p.dbReads = append(p.dbReads, call)
			} else {
				p.otherReads[base]++
			}
		}
	}
	for _, want := range []string{"open", "first statement", "close"} {
		if out[want] == nil {
			return nil, fmt.Errorf("missing marker %q", want)
		}
	}
	return out, nil
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child-trace-open" {
		childTraceOpen(os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "child-dirty-writer" {
		childDirtyWriter(os.Args[2])
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch08", "open-settings-edge")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// OPN-01: Missing file with and without the create flag (OUTLINE 8.1).
	{
		dir := filepath.Join(scratch, "opn01")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir opn01", err)
		}

		// mode=rw (no create)
		rwPath := filepath.Join(dir, "missing-rw.db")
		db, conn, rwErr := openPlain(ctx, rwPath, url.Values{"mode": []string{"rw"}})
		closePlain(db, conn)
		rwCode, rwHasCode := sqlitex.ErrorCode(rwErr)
		_, rwStatErr := os.Stat(rwPath)

		// mode=ro (no create)
		roPath := filepath.Join(dir, "missing-ro.db")
		db, conn, roErr := openPlain(ctx, roPath, url.Values{"mode": []string{"ro"}})
		closePlain(db, conn)
		roCode, roHasCode := sqlitex.ErrorCode(roErr)
		_, roStatErr := os.Stat(roPath)

		// mode=rwc (with create)
		rwcPath := filepath.Join(dir, "create-rwc.db")
		db, conn, rwcErr := openPlain(ctx, rwcPath, url.Values{"mode": []string{"rwc"}})
		if rwcErr != nil {
			fatal("open rwc", rwcErr)
		}
		infoAtOpen, errAtOpen := os.Stat(rwcPath)
		sizeAtOpen := int64(-1)
		if errAtOpen == nil {
			sizeAtOpen = infoAtOpen.Size()
		}
		if _, err := conn.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
			fatal("create table rwc", err)
		}
		infoAfterStmt, errAfterStmt := os.Stat(rwcPath)
		sizeAfterStmt := int64(-1)
		if errAfterStmt == nil {
			sizeAfterStmt = infoAfterStmt.Size()
		}
		closePlain(db, conn)

		held := rwHasCode && (rwCode&0xff) == 14 && errors.Is(rwStatErr, os.ErrNotExist) &&
			roHasCode && (roCode&0xff) == 14 && errors.Is(roStatErr, os.ErrNotExist) &&
			errAtOpen == nil && sizeAtOpen == 0 && errAfterStmt == nil && sizeAfterStmt >= pageSize
		report("OPN-01",
			"open missing file with mode=rw, mode=ro (no create) vs mode=rwc (create)",
			"mode=rw and mode=ro fail at open with SQLITE_CANTOPEN(14) without creating files; mode=rwc creates a 0-byte file at open and populates pages on first write",
			fmt.Sprintf("rw=code(%d,exists=%v) ro=code(%d,exists=%v) rwc=(size_at_open=%d,size_after_ddl=%d)",
				rwCode, !errors.Is(rwStatErr, os.ErrNotExist),
				roCode, !errors.Is(roStatErr, os.ErrNotExist),
				sizeAtOpen, sizeAfterStmt),
			held)
	}

	// OPN-02: Non-database files — error arrives at first statement, not at open (OUTLINE 8.1).
	{
		dir := filepath.Join(scratch, "opn02")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir opn02", err)
		}
		randBuf := make([]byte, 4096)
		if _, err := rand.Read(randBuf); err != nil {
			fatal("rand", err)
		}
		// Ensure random buffer does not accidentally start with SQLite magic.
		copy(randBuf[:16], []byte("NOT_SQLITE_RAND!"))

		cases := []struct {
			name string
			data []byte
		}{
			{"random-4096", randBuf},
			{"ascii-text", []byte(strings.Repeat("hello not a sqlite database\n", 10))},
			{"truncated-50", []byte("SQLite format 3\x00short-truncated-file-50-bytes!!!!!")},
		}
		allOK := true
		var parts []string
		for _, tc := range cases {
			p := filepath.Join(dir, tc.name+".db")
			if err := os.WriteFile(p, tc.data, 0o644); err != nil {
				fatal("write "+tc.name, err)
			}
			db, conn, openErr := openPlain(ctx, p, nil)
			if openErr != nil {
				allOK = false
				parts = append(parts, fmt.Sprintf("%s:plain_open_err=%v", tc.name, openErr))
				continue
			}
			var n int
			stmtErr := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n)
			closePlain(db, conn)
			code, hasCode := sqlitex.ErrorCode(stmtErr)

			// Contrast: sqlitex.OpenDefault runs PRAGMAs during Open so it fails at open.
			sxDB, sxErr := sqlitex.OpenDefault(ctx, p)
			if sxDB != nil {
				sxDB.Close()
			}
			sxCode, sxHasCode := sqlitex.ErrorCode(sxErr)

			ok := openErr == nil && hasCode && (code&0xff) == 26 && sxHasCode && (sxCode&0xff) == 26
			if !ok {
				allOK = false
			}
			parts = append(parts, fmt.Sprintf("%s(plain_open=ok,stmt_code=%d,sqlitex_open_code=%d)", tc.name, code, sxCode))
		}

		// Contrast: 0-byte empty file is a valid empty database.
		emptyPath := filepath.Join(dir, "empty-0.db")
		if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
			fatal("write empty", err)
		}
		db, conn, openErr := openPlain(ctx, emptyPath, nil)
		var emptyCount int
		stmtErr := error(nil)
		if openErr == nil {
			stmtErr = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&emptyCount)
			closePlain(db, conn)
		}
		emptyOK := openErr == nil && stmtErr == nil && emptyCount == 0
		if !emptyOK {
			allOK = false
		}
		parts = append(parts, fmt.Sprintf("empty-0(open=%v,stmt=%v,count=%d)", openErr, stmtErr, emptyCount))

		report("OPN-02",
			"open non-database files (random 4KiB, ASCII text, truncated 50B) and 0-byte file",
			"plain open succeeds on all non-DB files and SQLITE_NOTADB(26) arrives on first statement (sqlitex.Open fails immediately with 26); 0-byte file opens and queries cleanly",
			strings.Join(parts, "; "),
			allOK)
	}

	// OPN-03: Strace read patterns outside 8.1's clean WAL path (rollback mode & dirty WAL).
	{
		dir := filepath.Join(scratch, "opn03")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir opn03", err)
		}
		if !trace.Supported {
			report("OPN-03a",
				"strace open and first statement on rollback-mode DELETE database",
				"Linux-only strace check (skipped on non-Linux)",
				"skipped: trace.Supported=false on "+runtime.GOOS,
				true)
			report("OPN-03b",
				"strace open and first statement on WAL database with uncheckpointed frames from killed writer",
				"Linux-only strace check (skipped on non-Linux)",
				"skipped: trace.Supported=false on "+runtime.GOOS,
				true)
		} else {
			// OPN-03a: Rollback-mode DELETE database reads page 1 ONCE (not twice) and opens no -wal/-shm.
			rbPath := filepath.Join(dir, "rollback.db")
			opts := sqlitex.DefaultOptions()
			opts.File.JournalMode = sqlitex.JournalDelete
			opts.SingleConnection = true
			db, err := sqlitex.Open(ctx, rbPath, opts)
			if err != nil {
				fatal("open rollback", err)
			}
			if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'a')"); err != nil {
				fatal("create rollback", err)
			}
			if err := db.Close(); err != nil {
				fatal("close rollback", err)
			}

			cmdArgs, err := trace.SelfCommand("child-trace-open", rbPath)
			if err != nil {
				fatal("self cmd 03a", err)
			}
			res, err := trace.Run(ctx, trace.Options{
				Command:  cmdArgs,
				Syscalls: []string{"open", "openat", "pread64", "read", "write", "close"},
			})
			if err != nil || res.ExitCode != 0 {
				fatal("strace 03a", fmt.Errorf("err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr))
			}
			ph, err := parsePhases(res.Log, dir)
			if err != nil {
				fatal("phases 03a", err)
			}
			openOK := len(ph["open"].opens) == 1 && len(ph["open"].dbReads) == 1 && ph["open"].dbReads[0].Offset == 0 && ph["open"].dbReads[0].Length == 100
			p1Reads := 0
			for _, r := range ph["first statement"].dbReads {
				if r.Offset == 0 && r.Length == pageSize {
					p1Reads++
				}
			}
			noSideOpens := len(ph["first statement"].opens) == 0
			report("OPN-03a",
				"strace open and first statement on rollback-mode (DELETE) database",
				"open reads only 100-byte header; first statement reads page 1 ONCE (unlike WAL's twice) and opens no -wal/-shm",
				fmt.Sprintf("open_reads=%d(len=%d) first_stmt_p1_reads=%d first_stmt_opens=%v",
					len(ph["open"].dbReads), ph["open"].dbReads[0].Length, p1Reads, ph["first statement"].opens),
				openOK && p1Reads == 1 && noSideOpens)

			// OPN-03b: Dirty WAL (killed writer left committed frames in -wal).
			dirtyPath := filepath.Join(dir, "dirty-wal.db")
			walOpts := sqlitex.DefaultOptions()
			walOpts.SingleConnection = true
			wdb, err := sqlitex.Open(ctx, dirtyPath, walOpts)
			if err != nil {
				fatal("open dirty wal", err)
			}
			if _, err := wdb.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'base')"); err != nil {
				fatal("seed dirty wal", err)
			}
			if err := wdb.Close(); err != nil {
				fatal("close dirty wal", err)
			}
			selfExe, err := os.Executable()
			if err != nil {
				fatal("executable", err)
			}
			wCmd := exec.CommandContext(ctx, selfExe, "child-dirty-writer", dirtyPath)
			wStdin, err := wCmd.StdinPipe()
			if err != nil {
				fatal("stdin pipe", err)
			}
			wStdout, err := wCmd.StdoutPipe()
			if err != nil {
				fatal("stdout pipe", err)
			}
			if err := wCmd.Start(); err != nil {
				fatal("start dirty writer", err)
			}
			buf := make([]byte, 64)
			n, err := wStdout.Read(buf)
			if err != nil || !strings.Contains(string(buf[:n]), "ready") {
				wCmd.Process.Kill()
				fatal("dirty writer ready", fmt.Errorf("read=%q err=%v", string(buf[:n]), err))
			}
			if err := wCmd.Process.Kill(); err != nil {
				fatal("kill dirty writer", err)
			}
			_ = wStdin.Close()
			_ = wCmd.Wait()

			walInfo, err := os.Stat(dirtyPath + "-wal")
			if err != nil || walInfo.Size() == 0 {
				fatal("dirty wal size", fmt.Errorf("stat=%v err=%v", walInfo, err))
			}

			cmdArgs, err = trace.SelfCommand("child-trace-open", dirtyPath)
			if err != nil {
				fatal("self cmd 03b", err)
			}
			res, err = trace.Run(ctx, trace.Options{
				Command:  cmdArgs,
				Syscalls: []string{"open", "openat", "pread64", "read", "write", "close"},
			})
			if err != nil || res.ExitCode != 0 {
				fatal("strace 03b", fmt.Errorf("err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr))
			}
			ph, err = parsePhases(res.Log, dir)
			if err != nil {
				fatal("phases 03b", err)
			}
			openOnlyHeader := len(ph["open"].opens) == 1 && len(ph["open"].dbReads) == 1 && ph["open"].dbReads[0].Length == 100 && len(ph["open"].otherReads) == 0
			walReadsAtFirst := ph["first statement"].otherReads[filepath.Base(dirtyPath)+"-wal"]
			report("OPN-03b",
				fmt.Sprintf("strace open and first statement on WAL DB with uncheckpointed -wal (%d bytes from SIGKILLed writer)", walInfo.Size()),
				"open still reads only the 100-byte .db header (no -wal/-shm touch); first statement DOES read -wal during recovery (scoping 8.1's zero-WAL-read check to at-rest files)",
				fmt.Sprintf("open_opens=%v open_db_reads=%d(len=%d) first_stmt_wal_reads=%d",
					ph["open"].opens, len(ph["open"].dbReads), ph["open"].dbReads[0].Length, walReadsAtFirst),
				openOnlyHeader && walReadsAtFirst > 0)
		}
	}

	// OPN-04: Settings persistence edge cases.
	{
		dir := filepath.Join(scratch, "opn04")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir opn04", err)
		}

		// OPN-04a: All 5 rollback journal modes reset to delete on reopen; WAL->delete flips header bytes 18-19 back to 1,1.
		rollbackOK := true
		var rbParts []string
		for _, mode := range sqlitex.RollbackJournalModes() {
			p := filepath.Join(dir, "rb-"+mode+".db")
			db, conn, err := openPlain(ctx, p, nil)
			if err != nil {
				fatal("open "+mode, err)
			}
			gotSet, err := readPragma(ctx, conn, "journal_mode="+mode)
			if err != nil {
				fatal("set "+mode, err)
			}
			if _, err := conn.ExecContext(ctx, "CREATE TABLE t(x); INSERT INTO t VALUES(1)"); err != nil {
				fatal("ddl "+mode, err)
			}
			closePlain(db, conn)

			db, conn, err = openPlain(ctx, p, nil)
			if err != nil {
				fatal("reopen "+mode, err)
			}
			reopened, err := readPragma(ctx, conn, "journal_mode")
			closePlain(db, conn)
			if err != nil {
				fatal("read "+mode, err)
			}
			hdr, err := os.ReadFile(p)
			if err != nil || len(hdr) < 100 {
				fatal("hdr "+mode, err)
			}
			ok := gotSet == mode && reopened == sqlitex.JournalDelete && hdr[18] == 1 && hdr[19] == 1
			if !ok {
				rollbackOK = false
			}
			rbParts = append(rbParts, fmt.Sprintf("%s->%s(hdr=%d,%d)", gotSet, reopened, hdr[18], hdr[19]))
		}
		// WAL -> DELETE revert test.
		walRevert := filepath.Join(dir, "wal-revert.db")
		wdb, err := sqlitex.OpenDefault(ctx, walRevert)
		if err != nil {
			fatal("open wal revert", err)
		}
		if _, err := wdb.ExecContext(ctx, "CREATE TABLE t(x); INSERT INTO t VALUES(1)"); err != nil {
			fatal("ddl wal revert", err)
		}
		if err := wdb.Close(); err != nil {
			fatal("close wal revert", err)
		}
		hdrBefore, _ := os.ReadFile(walRevert)
		db, conn, err := openPlain(ctx, walRevert, nil)
		if err != nil {
			fatal("reopen wal revert", err)
		}
		reverted, err := readPragma(ctx, conn, "journal_mode=delete")
		if err != nil {
			fatal("revert wal", err)
		}
		closePlain(db, conn)
		hdrAfter, _ := os.ReadFile(walRevert)
		revertOK := hdrBefore[18] == 2 && hdrBefore[19] == 2 && reverted == sqlitex.JournalDelete && hdrAfter[18] == 1 && hdrAfter[19] == 1
		report("OPN-04a",
			"verify all 5 rollback journal modes do not persist on reopen, and switching WAL back to delete resets header bytes 18-19",
			"every rollback mode reads back delete with header 1,1; WAL(2,2)->delete resets header to 1,1",
			fmt.Sprintf("rollback=[%s] wal_revert=(%d,%d->%s:%d,%d)",
				strings.Join(rbParts, " "), hdrBefore[18], hdrBefore[19], reverted, hdrAfter[18], hdrAfter[19]),
			rollbackOK && revertOK)

		// OPN-04b: Negative PRAGMA default_cache_size = -500 (testing build-notes #2's unmeasured source claim).
		negPath := filepath.Join(dir, "neg-default-cache.db")
		db, conn, err = openPlain(ctx, negPath, nil)
		if err != nil {
			fatal("open neg cache", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA default_cache_size=-500; CREATE TABLE t(x)"); err != nil {
			fatal("set neg cache", err)
		}
		closePlain(db, conn)
		negHdr, err := os.ReadFile(negPath)
		if err != nil || len(negHdr) < 100 {
			fatal("read neg hdr", err)
		}
		storedSigned := int32(binary.BigEndian.Uint32(negHdr[48:52]))
		db, conn, err = openPlain(ctx, negPath, nil)
		if err != nil {
			fatal("reopen neg cache", err)
		}
		gotDefCache, err := readPragma(ctx, conn, "default_cache_size")
		if err != nil {
			fatal("read def cache", err)
		}
		gotCache, err := readPragma(ctx, conn, "cache_size")
		closePlain(db, conn)
		if err != nil {
			fatal("read cache", err)
		}
		// prepare.c:325 takes sqlite3AbsInt32(meta[BTREE_DEFAULT_CACHE_SIZE-1]) -> +500 pages!
		report("OPN-04b",
			"set PRAGMA default_cache_size=-500, close, reopen plain connection (testing build-notes #2 source claim)",
			"header bytes 48-51 store -500 (or 500) and sqlite3InitOne applies AbsInt32 so reopened connection starts with positive cache_size=500 pages",
			fmt.Sprintf("header[48:52]=%d reopened_default_cache_size=%s reopened_cache_size=%s",
				storedSigned, gotDefCache, gotCache),
			gotCache == "500")

		// OPN-04c: VACUUM INTO vs default_cache_size=777 (testing 8.1's claim "VACUUM INTO keeps every file setting except journal_mode").
		vacSrc := filepath.Join(dir, "vac-defcache-src.db")
		vacDst := filepath.Join(dir, "vac-defcache-dst.db")
		db, conn, err = openPlain(ctx, vacSrc, nil)
		if err != nil {
			fatal("open vac src", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA default_cache_size=777; CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES (1, 'one')"); err != nil {
			fatal("seed vac src", err)
		}
		if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", vacDst); err != nil {
			fatal("vacuum into", err)
		}
		closePlain(db, conn)

		srcHdr, _ := os.ReadFile(vacSrc)
		dstHdr, _ := os.ReadFile(vacDst)
		srcStored := int32(binary.BigEndian.Uint32(srcHdr[48:52]))
		dstStored := int32(binary.BigEndian.Uint32(dstHdr[48:52]))
		db, conn, err = openPlain(ctx, vacDst, nil)
		if err != nil {
			fatal("open vac dst", err)
		}
		vacDefCache, _ := readPragma(ctx, conn, "default_cache_size")
		vacCache, _ := readPragma(ctx, conn, "cache_size")
		closePlain(db, conn)

		// sqlitex.SettingPlacements() classifies default_cache_size as SettingInFile, and 8.1 claims
		// "VACUUM INTO keeps every file setting except journal_mode".
		report("OPN-04c",
			"run VACUUM INTO on a DB with PRAGMA default_cache_size=777 (classified SettingInFile by sqlitex.SettingPlacements)",
			"per 8.1's claim ('VACUUM INTO keeps every file setting except journal_mode'), VACUUM INTO output preserves header[48:52]=777 and cache_size=777",
			fmt.Sprintf("src_hdr[48:52]=%d dst_hdr[48:52]=%d dst_default_cache_size=%s dst_cache_size=%s",
				srcStored, dstStored, vacDefCache, vacCache),
			dstStored == 777 && vacCache == "777")
	}

	// OPN-05: Machine-readable settings-json audit of Experiment 8.1 output.
	{
		expWork := filepath.Join(scratch, "exp81-work")
		res, err := runGoPkg(ctx, repoRoot, "./ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live", []string{"COURSE_WORK_DIR=" + expWork})
		if err != nil {
			fatal("run 8.1", err)
		}
		if res.exitCode != 0 {
			fatal("8.1 exit", fmt.Errorf("exit=%d stdout=%s stderr=%s", res.exitCode, res.stdout, res.stderr))
		}
		var jsonLine string
		for _, line := range strings.Split(res.stdout, "\n") {
			if strings.HasPrefix(line, "settings-json: ") {
				jsonLine = strings.TrimPrefix(line, "settings-json: ")
				break
			}
		}
		if jsonLine == "" {
			report("OPN-05",
				"parse settings-json emitted by Experiment 8.1",
				"valid JSON with populated placements and every observed[i].placement matching SettingPlacements",
				"no settings-json line found in 8.1 output",
				false)
		} else {
			var doc struct {
				Placements []sqlitex.SettingPlacement `json:"placements"`
				Observed   []struct {
					Setting    string                   `json:"setting"`
					Home       sqlitex.SettingHome      `json:"home"`
					Set        string                   `json:"set_on_original"`
					Default    string                   `json:"fresh_connection_default"`
					Copy       string                   `json:"read_on_copy"`
					VacuumInto string                   `json:"read_on_vacuum_into"`
					Placement  sqlitex.SettingPlacement `json:"placement"`
				} `json:"observed"`
			}
			if err := json.Unmarshal([]byte(jsonLine), &doc); err != nil {
				report("OPN-05",
					"parse settings-json emitted by Experiment 8.1",
					"valid JSON with populated placements and observed[i].placement fields",
					fmt.Sprintf("unmarshal error: %v", err),
					false)
			} else {
				emptyPlacements := 0
				mismatched := 0
				for _, o := range doc.Observed {
					if o.Placement.Name == "" || o.Placement.Pragma == "" || o.Placement.Home == "" {
						emptyPlacements++
						continue
					}
					want, ok := sqlitex.SettingPlacementFor(o.Setting)
					if !ok || o.Placement != want {
						mismatched++
					}
				}
				held := len(doc.Placements) == len(sqlitex.SettingPlacements()) &&
					len(doc.Observed) == 12 &&
					emptyPlacements == 0 &&
					mismatched == 0
				report("OPN-05",
					"audit Experiment 8.1 settings-json machine-readable output",
					"all 14 placements present and all 12 observed[i].placement structs populated to match sqlitex.SettingPlacementFor",
					fmt.Sprintf("placements=%d observed=%d empty_observed_placements=%d mismatched=%d",
						len(doc.Placements), len(doc.Observed), emptyPlacements, mismatched),
					held)
			}
		}
	}

	// OPN-06 ("must be able to fail"): Pin-check enforcement across all 3 ch08 experiments.
	{
		scratchRepo := filepath.Join(scratch, "scratch-repo")
		if err := buildScratchRepo(repoRoot, scratchRepo); err != nil {
			fatal("build scratch repo", err)
		}
		pinPath := filepath.Join(scratchRepo, "sqlite", "PIN")
		pinBytes, err := os.ReadFile(pinPath)
		if err != nil {
			fatal("read PIN", err)
		}
		mutated := strings.ReplaceAll(string(pinBytes),
			"bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc",
			"0000000000000000000000000000000000000000000000000000000000000000")
		if err := os.WriteFile(pinPath, []byte(mutated), 0o644); err != nil {
			fatal("write mutated PIN", err)
		}
		workDir := filepath.Join(scratch, "pin-work")
		exps := []struct {
			name string
			pkg  string
		}{
			{"8.1", "./ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live"},
			{"8.2", "./ch08-connection-and-database-lifecycle/experiments/8.2-idle-cost"},
			{"8.3", "./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager"},
		}
		var failed []string
		var summary []string
		for _, e := range exps {
			res, err := runGoPkg(ctx, scratchRepo, e.pkg, []string{"COURSE_WORK_DIR=" + workDir})
			if err != nil {
				fatal("run "+e.name, err)
			}
			l1 := firstLine(res.stdout)
			ok := res.exitCode != 0 && strings.HasPrefix(l1, "sqlite_source_id:") && strings.Contains(l1, "NOT the pin")
			summary = append(summary, fmt.Sprintf("%s:exit=%d", e.name, res.exitCode))
			if !ok {
				failed = append(failed, fmt.Sprintf("%s(exit=%d,line1=%q)", e.name, res.exitCode, l1))
			}
		}
		report("OPN-06",
			"mutate sqlite/PIN in scratch repo and run ch08 experiments 8.1, 8.2, 8.3",
			"all 3 experiments exit non-zero and report sqlite_source_id mismatch on line 1",
			fmt.Sprintf("results=[%s] failures=%v", strings.Join(summary, ", "), failed),
			len(failed) == 0)
	}
}
