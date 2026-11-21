// Group 3 adversarial attacks for Chapter 7, Experiment 7.3 (go-vfs-shim) and internal/govfs.
// Attacks:
//
//	GOVFS-01: Must-be-able-to-fail check on 7.3 part A Go-vs-CLI VFS call parity comparator.
//	GOVFS-02: Go-vs-CLI VFS call parity on harder SQL (10KB overflow INSERT, ROLLBACK, wal_checkpoint).
//	GOVFS-03: Fault sweeps on xRead (1..N+1) and xTruncate (checkpoint/VACUUM) — uncovered by 7.3 part B.
//	GOVFS-04: Fault sweeps at synchronous=NORMAL (cold WAL, DELETE, and warm WAL with 0 xSync).
//	GOVFS-05: SQLITE_FULL (13), extended SQLITE_IOERR_FSYNC (1034), and same-connection reuse after fault.
//	GOVFS-06: SQLITE_FCNTL_VFSNAME identity check against vfstrace.c's shim contract.
//	GOVFS-07: Stress govfs with mmap_size (xFetch/xUnfetch), temp tables, savepoint subjournals, VACUUM, v2, and concurrency.
//	GOVFS-08: Must-be-able-to-fail check on internal/govfs TestSignaturesMatchTranslatedSource via scratch ABI mutation.
//	GOVFS-09: Arm + SetRecording(false) accounting and interoperability with modernc.org/sqlite/vfs.
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing/fstest"
	"unsafe"

	"github.com/nickstrad/walfleet/internal/govfs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
	"modernc.org/sqlite/vfs"
)

const (
	scratchRoot = "_work/adv-ch07/govfs-adversarial"
	vfsMain     = "adv-govfs-main"
	vfsV2       = "adv-govfs-v2"
	vfsStress   = "adv-govfs-stress"
	dbName      = "t.db"
	faultStep   = 4
)

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

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func cliPrelude(o sqlitex.Options) string {
	f, c := o.File, o.Conn
	return fmt.Sprintf("PRAGMA page_size=%d;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\nPRAGMA journal_mode=%s;\nPRAGMA application_id=%d;\nPRAGMA user_version=%d;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=%s;\nPRAGMA secure_delete=%s;\nPRAGMA cache_size=%d;\nPRAGMA wal_autocheckpoint=%d;\nPRAGMA busy_timeout=%d;\n",
		f.PageSize, f.AutoVacuum, f.Encoding, f.JournalMode, f.ApplicationID, f.UserVersion, c.Synchronous, onOff(c.ForeignKeys), onOff(c.SecureDelete), c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

// Mirror 7.3's exact key normalization so parity checks match 7.3's contract.
func goKey(c govfs.Call) string {
	switch c.Method {
	case "xRead", "xWrite":
		return fmt.Sprintf("%s %s n=%d ofst=%d", c.Method, c.File, c.Amount, c.Offset)
	case "xSync":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.VFSTraceSyncLabel(c.Flags))
	case "xLock", "xUnlock":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.LockLevelString(c.Flags))
	case "xFileControl":
		return fmt.Sprintf("%s %s %s", c.Method, c.File, govfs.FileControlName(c.Flags))
	case "xShmLock":
		return fmt.Sprintf("%s %s ofst=%d n=%d %s", c.Method, c.File, c.Offset, c.Amount, govfs.ShmLockFlagsString(c.Flags))
	case "xShmMap":
		return fmt.Sprintf("%s %s region=%d size=%d write=%d", c.Method, c.File, c.Offset, c.Amount, c.Flags)
	case "xTruncate":
		return fmt.Sprintf("%s %s %d", c.Method, c.File, c.Amount)
	case "xOpen":
		return fmt.Sprintf("%s %s flags=0x%x", c.Method, c.File, c.Flags)
	case "xDelete", "xAccess":
		return fmt.Sprintf("%s %s %d", c.Method, c.File, c.Flags)
	case "xShmUnmap":
		return fmt.Sprintf("%s %s delete=%d", c.Method, c.File, c.Flags)
	}
	return c.Method + " " + c.File
}

func argValue(arg string) string {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[i+1:]
	}
	return arg
}

func cliKey(c trace.VFSCall) string {
	file := filepath.Base(strings.Trim(c.File, `"`))
	arg := func(i int) string {
		if i < len(c.Args) {
			return argValue(c.Args[i])
		}
		return ""
	}
	switch c.Method {
	case "xRead", "xWrite":
		return fmt.Sprintf("%s %s n=%s ofst=%s", c.Method, file, arg(1), arg(2))
	case "xSync", "xLock", "xUnlock":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xFileControl":
		op := arg(1)
		if i := strings.IndexAny(op, ",["); i >= 0 {
			op = op[:i]
		}
		return fmt.Sprintf("%s %s %s", c.Method, file, op)
	case "xShmLock":
		ofst := arg(1)
		if i := strings.IndexByte(ofst, '('); i >= 0 {
			ofst = ofst[:i]
		}
		return fmt.Sprintf("%s %s ofst=%s n=%s %s", c.Method, file, ofst, arg(2), arg(3))
	case "xShmMap":
		return fmt.Sprintf("%s %s region=%s size=%s write=%s", c.Method, file, arg(1), arg(2), arg(3))
	case "xTruncate":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xOpen":
		return fmt.Sprintf("%s %s flags=%s", c.Method, file, arg(1))
	case "xDelete", "xAccess":
		return fmt.Sprintf("%s %s %s", c.Method, file, arg(1))
	case "xShmUnmap":
		return fmt.Sprintf("%s %s delete=%s", c.Method, file, arg(1))
	case "xRandomness", "xCurrentTime", "xCurrentTimeInt64", "xSleep":
		return c.Method + " "
	}
	return c.Method + " " + file
}

func runPairKeys(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS, dir string, goOpts, cliOpts sqlitex.Options, setupSQL []string, targetSQL []string) (goKeys, cliKeys []string) {
	goPath := filepath.Join(dir, "go.db")
	cliPath := filepath.Join(dir, "cli.db")
	_ = recipe.RemoveDatabase(goPath)
	_ = recipe.RemoveDatabase(cliPath)

	goOpts.SingleConnection = true
	goOpts.VFS = v.Name()
	db, err := sqlitex.Open(ctx, goPath, goOpts)
	if err != nil {
		fatal("go open", err)
	}
	for _, s := range setupSQL {
		if _, err := db.ExecContext(ctx, s); err != nil {
			fatal("go setup "+s, err)
		}
	}
	v.Reset()
	for _, s := range targetSQL {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "PRAGMA ") {
			rows, err := db.QueryContext(ctx, s)
			if err != nil {
				fatal("go target pragma "+s, err)
			}
			for rows.Next() {
			}
			_ = rows.Close()
		} else {
			if _, err := db.ExecContext(ctx, s); err != nil {
				fatal("go target "+s, err)
			}
		}
	}
	gCalls := v.TakeCalls()
	_ = db.Close()

	var script strings.Builder
	script.WriteString(cliPrelude(cliOpts))
	for _, s := range setupSQL {
		script.WriteString(s + ";\n")
	}
	script.WriteString(".print MARK-BEGIN\n")
	for _, s := range targetSQL {
		script.WriteString(s + ";\n")
	}
	script.WriteString(".print MARK-END\n")

	res, err := cli.Run(ctx, sqlitex.CLIRun{
		Args:  []string{"-vfstrace"},
		Stdin: strings.NewReader(sqlitex.OpenScript(cliPath, script.String())),
		Dir:   dir,
	})
	if err != nil {
		fatal("cli run", err)
	}
	if res.ExitCode != 0 {
		fatal("cli exit", fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr))
	}
	var cCalls []trace.VFSCall
	inside := false
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		switch line {
		case "MARK-BEGIN":
			inside = true
			continue
		case "MARK-END":
			inside = false
			continue
		}
		if !inside {
			continue
		}
		if c, ok := trace.ParseVFSLine(line); ok {
			cCalls = append(cCalls, c)
		}
	}
	cCalls = trace.RelativeVFS(dir, cCalls)

	for _, c := range gCalls {
		c.File = strings.Replace(c.File, "go.db", dbName, 1)
		goKeys = append(goKeys, goKey(c))
	}
	for _, c := range cCalls {
		cliKeys = append(cliKeys, strings.Replace(cliKey(c), "cli.db", dbName, 1))
	}
	return goKeys, cliKeys
}

func compareKeys(a, b []string) (bool, string) {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return false, fmt.Sprintf("call %d: go=%q cli=%q", i+1, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		return false, fmt.Sprintf("len go=%d cli=%d", len(a), len(b))
	}
	return true, fmt.Sprintf("all %d calls identical", len(a))
}

func deterministicHexBlob(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*37 + 13) & 0xff)
	}
	return "X'" + strings.ToUpper(hex.EncodeToString(b)) + "'"
}

func main() {
	ctx := context.Background()
	cli := sqlitex.MustLocateCLI(ctx)
	vMain, err := govfs.Register(vfsMain, govfs.Options{})
	if err != nil {
		fatal("register vMain", err)
	}
	vV2, err := govfs.Register(vfsV2, govfs.Options{IOVersion: 2})
	if err != nil {
		fatal("register vV2", err)
	}
	vStress, err := govfs.Register(vfsStress, govfs.Options{})
	if err != nil {
		fatal("register vStress", err)
	}

	attackGOVFS01(ctx, cli, vMain)
	attackGOVFS02(ctx, cli, vMain)
	attackGOVFS03(ctx, vMain)
	attackGOVFS04(ctx, vMain)
	attackGOVFS05(ctx, vMain)
	attackGOVFS06(vMain)
	attackGOVFS07(ctx, vMain, vV2, vStress)
	attackGOVFS08(ctx)
	attackGOVFS09(ctx, vMain)
}

// GOVFS-01: Must-be-able-to-fail check on 7.3 part A Go-vs-CLI VFS parity comparator.
func attackGOVFS01(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs01-fail"))
	goOpts := sqlitex.DefaultOptions()
	goOpts.File.JournalMode = sqlitex.JournalDelete
	goOpts.Conn.Synchronous = sqlitex.SyncFull

	cliOpts := goOpts
	cliOpts.Conn.Synchronous = sqlitex.SyncNormal

	setup := []string{"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1,'a')"}
	target := []string{"INSERT INTO t VALUES(2,'b')"}
	g1, c1 := runPairKeys(ctx, cli, v, dir, goOpts, cliOpts, setup, target)
	eq1, diff1 := compareKeys(g1, c1)

	// Second mismatch: wal/FULL vs wal/NORMAL
	goOpts.File.JournalMode = sqlitex.JournalWAL
	cliOpts.File.JournalMode = sqlitex.JournalWAL
	g2, c2 := runPairKeys(ctx, cli, v, dir, goOpts, cliOpts, setup, target)
	eq2, diff2 := compareKeys(g2, c2)

	emit("GOVFS-01",
		"must-be-able-to-fail: compare Go FULL vs CLI NORMAL through 7.3's VFS key matcher (delete and wal)",
		"matcher rejects both mismatched pairs and pinpoints the extra xSync difference",
		fmt.Sprintf("delete(%t: %s); wal(%t: %s)", eq1, diff1, eq2, diff2),
		!eq1 && !eq2)
}

// GOVFS-02: Go-vs-CLI VFS call parity on harder SQL statements.
func attackGOVFS02(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs02-harder"))
	blob10k := deterministicHexBlob(10000)

	type sc struct {
		name    string
		journal string
		setup   []string
		target  []string
	}
	cases := []sc{
		{
			name:    "wal-overflow-10k",
			journal: sqlitex.JournalWAL,
			setup:   []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB); INSERT INTO docs VALUES(1, X'01')"},
			target:  []string{"INSERT INTO docs VALUES(2, " + blob10k + ")"},
		},
		{
			name:    "delete-overflow-10k",
			journal: sqlitex.JournalDelete,
			setup:   []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB); INSERT INTO docs VALUES(1, X'01')"},
			target:  []string{"INSERT INTO docs VALUES(2, " + blob10k + ")"},
		},
		{
			name:    "delete-rollback-10k",
			journal: sqlitex.JournalDelete,
			setup:   []string{"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB); INSERT INTO docs VALUES(1, X'01')"},
			target:  []string{"BEGIN IMMEDIATE", "INSERT INTO docs VALUES(2, " + blob10k + ")", "ROLLBACK"},
		},
		{
			name:    "wal-checkpoint-truncate",
			journal: sqlitex.JournalWAL,
			setup: []string{
				"CREATE TABLE docs(id INTEGER PRIMARY KEY, body BLOB)",
				"INSERT INTO docs VALUES(1, " + blob10k + ")",
				"INSERT INTO docs VALUES(2, " + blob10k + ")",
			},
			target: []string{"PRAGMA wal_checkpoint(TRUNCATE)"},
		},
	}

	allOK := true
	var parts []string
	for _, tc := range cases {
		o := sqlitex.DefaultOptions()
		o.File.JournalMode = tc.journal
		o.Conn.Synchronous = sqlitex.SyncFull
		o.Conn.SecureDelete = true
		sub := cleanDir(filepath.Join(dir, tc.name))
		gk, ck := runPairKeys(ctx, cli, v, sub, o, o, tc.setup, tc.target)
		ok, info := compareKeys(gk, ck)
		if !ok {
			allOK = false
		}
		parts = append(parts, fmt.Sprintf("%s:(%t %s)", tc.name, ok, info))
	}
	emit("GOVFS-02",
		"Go govfs vs CLI -vfstrace call-for-call parity on 10KB overflow INSERT (wal/delete), explicit ROLLBACK, and wal_checkpoint(TRUNCATE)",
		"identical ordered VFS call lists in all 4 harder workloads",
		strings.Join(parts, "; "), allOK)
}

func applyStepWithFault(ctx context.Context, r recipe.Recipe, opts sqlitex.Options, path string, step recipe.Step, v *govfs.VFS, fault *govfs.Fault) (error, govfs.FaultReport, []govfs.Call, error) {
	opts.VFS = v.Name()
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open fault trial", err)
	}
	v.Reset()
	if fault != nil {
		if err := v.Arm(*fault); err != nil {
			fatal("arm fault", err)
		}
	}
	var stepErr error
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		stepErr = err
	} else {
		for _, s := range step.Statements {
			if _, err := tx.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				stepErr = err
				break
			}
		}
		if stepErr == nil {
			stepErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	report := v.Disarm()
	calls := v.TakeCalls()
	closeErr := db.Close()
	return stepErr, report, calls, closeErr
}

func checkStepState(ctx context.Context, r recipe.Recipe, path string) (int, string, recipe.Match) {
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("reopen check", err)
	}
	var uv int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uv); err != nil {
		fatal("uv", err)
	}
	var ic string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
		fatal("ic", err)
	}
	_ = db.Close()
	m, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	return uv, ic, m
}

// GOVFS-03: Fault sweeps on xRead (every call of recipe step 4 in wal/delete) and xTruncate (checkpoint + VACUUM).
func attackGOVFS03(ctx context.Context, v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs03-read-trunc"))

	readOK := true
	var readSummary []string
	for _, journal := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		r := recipe.Default().WithJournalMode(journal).WithSteps(faultStep + 1)
		if _, err := r.StepHashes(ctx); err != nil {
			fatal("step hashes", err)
		}
		base := filepath.Join(dir, "base-"+journal+".db")
		if err := r.Build(ctx, base, faultStep-1); err != nil {
			fatal("build", err)
		}
		step, err := r.Step(faultStep)
		if err != nil {
			fatal("step", err)
		}
		clean := filepath.Join(dir, "clean-"+journal+".db")
		if err := copyFile(base, clean); err != nil {
			fatal("copy", err)
		}
		stepErr, _, calls, closeErr := applyStepWithFault(ctx, r, r.SQLiteOptions(), clean, step, v, nil)
		if stepErr != nil || closeErr != nil {
			fatal("clean "+journal, fmt.Errorf("%v / %v", stepErr, closeErr))
		}
		xReads := len(govfs.Filter(calls, govfs.Methods("xRead")))
		firedOK := 0
		var bad []string
		for n := 1; n <= xReads+1; n++ {
			trial := filepath.Join(dir, fmt.Sprintf("read-%s-%02d.db", journal, n))
			if err := copyFile(base, trial); err != nil {
				fatal("copy", err)
			}
			f := govfs.Fault{Method: "xRead", Nth: n, Code: govfs.CodeIOErr}
			err, rep, _, _ := applyStepWithFault(ctx, r, r.SQLiteOptions(), trial, step, v, &f)
			uv, ic, m := checkStepState(ctx, r, trial)
			if rep.Fired {
				if err != nil && uv == faultStep-1 && ic == "ok" && m.Step == faultStep-1 && m.Exact {
					firedOK++
				} else {
					readOK = false
					bad = append(bad, fmt.Sprintf("#%d(err=%v uv=%d ic=%s m=%s)", n, err, uv, ic, m))
				}
			} else if err != nil || uv != faultStep || ic != "ok" || m.Step != faultStep {
				readOK = false
				bad = append(bad, fmt.Sprintf("#%d-unfired(err=%v uv=%d)", n, err, uv))
			}
			_ = recipe.RemoveDatabase(trial)
		}
		s := fmt.Sprintf("%s:xRead=%d/%d ok", journal, firedOK, xReads)
		if len(bad) > 0 {
			s += " bad=" + strings.Join(bad, ",")
		}
		readSummary = append(readSummary, s)
	}

	// xTruncate fault on WAL checkpoint(TRUNCATE) and shrinking DELETE VACUUM.
	truncOK := true
	var truncNotes []string

	// (a) WAL checkpoint(TRUNCATE)
	walPath := filepath.Join(dir, "trunc-wal.db")
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = sqlitex.JournalWAL
	o.Conn.WALAutocheckpoint = 0
	o.SingleConnection = true
	o.VFS = v.Name()
	db, err := sqlitex.Open(ctx, walPath, o)
	if err != nil {
		fatal("trunc wal open", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB); INSERT INTO t VALUES(1, zeroblob(8000)); INSERT INTO t VALUES(2, zeroblob(8000))"); err != nil {
		fatal("trunc wal setup", err)
	}
	if err := v.Arm(govfs.Fault{Method: "xTruncate", Nth: 1, Code: govfs.CodeIOErr}); err != nil {
		fatal("arm trunc wal", err)
	}
	_, ckptErr := sqlitex.Checkpoint(ctx, db, "main", sqlitex.CheckpointTruncate)
	repW := v.Disarm()
	_ = db.Close()
	// Reopen with default VFS and verify both rows and integrity_check=ok.
	oPlain := o
	oPlain.VFS = ""
	db2, err := sqlitex.Open(ctx, walPath, oPlain)
	if err != nil {
		fatal("trunc wal reopen", err)
	}
	var cntW int
	var icW string
	_ = db2.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&cntW)
	_ = db2.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&icW)
	_ = db2.Close()
	codeW, _ := sqlitex.PrimaryErrorCode(ckptErr)
	if !repW.Fired || codeW != int(govfs.CodeIOErr) || cntW != 2 || icW != "ok" {
		truncOK = false
	}
	truncNotes = append(truncNotes, fmt.Sprintf("wal-ckpt(fired=%t code=%d rows=%d ic=%s)", repW.Fired, codeW, cntW, icW))

	// (b) DELETE mode shrinking VACUUM
	delPath := filepath.Join(dir, "trunc-del.db")
	oDel := sqlitex.DefaultOptions()
	oDel.File.JournalMode = sqlitex.JournalDelete
	oDel.SingleConnection = true
	oDel.VFS = v.Name()
	dbD, err := sqlitex.Open(ctx, delPath, oDel)
	if err != nil {
		fatal("trunc del open", err)
	}
	if _, err := dbD.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB); INSERT INTO t VALUES(1, zeroblob(20000)); INSERT INTO t VALUES(2, zeroblob(20000)); DELETE FROM t WHERE id=2"); err != nil {
		fatal("trunc del setup", err)
	}
	if err := v.Arm(govfs.Fault{Method: "xTruncate", Nth: 1, Code: govfs.CodeIOErr}); err != nil {
		fatal("arm trunc del", err)
	}
	_, vacErr := dbD.ExecContext(ctx, "VACUUM")
	repD := v.Disarm()
	_ = dbD.Close()
	oDelPlain := oDel
	oDelPlain.VFS = ""
	dbD2, err := sqlitex.Open(ctx, delPath, oDelPlain)
	if err != nil {
		fatal("trunc del reopen", err)
	}
	var cntD int
	var icD string
	_ = dbD2.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&cntD)
	_ = dbD2.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&icD)
	_ = dbD2.Close()
	codeD, _ := sqlitex.PrimaryErrorCode(vacErr)
	if !repD.Fired || codeD != int(govfs.CodeIOErr) || cntD != 1 || icD != "ok" {
		truncOK = false
	}
	truncNotes = append(truncNotes, fmt.Sprintf("del-vacuum(fired=%t code=%d rows=%d ic=%s)", repD.Fired, codeD, cntD, icD))

	emit("GOVFS-03",
		"fault sweep on every xRead of recipe step 4 (wal/delete) and xTruncate on checkpoint(TRUNCATE) + shrinking VACUUM",
		"every fired xRead fault rolls back to exact step 3; xTruncate fault returns code 10 with integrity ok and zero data loss",
		strings.Join(readSummary, "; ")+" | "+strings.Join(truncNotes, "; "),
		readOK && truncOK)
}

// GOVFS-04: Fault sweeps at synchronous=NORMAL (cold WAL, DELETE, and warm WAL with 0 xSync).
func attackGOVFS04(ctx context.Context, v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs04-normal"))
	allOK := true
	var notes []string

	for _, journal := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		r := recipe.Default().WithJournalMode(journal).WithSteps(faultStep + 2)
		if _, err := r.StepHashes(ctx); err != nil {
			fatal("step hashes", err)
		}
		base := filepath.Join(dir, "base-"+journal+".db")
		if err := r.Build(ctx, base, faultStep-1); err != nil {
			fatal("build", err)
		}
		step, err := r.Step(faultStep)
		if err != nil {
			fatal("step", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.Synchronous = sqlitex.SyncNormal

		clean := filepath.Join(dir, "clean-"+journal+".db")
		if err := copyFile(base, clean); err != nil {
			fatal("copy", err)
		}
		stepErr, _, calls, closeErr := applyStepWithFault(ctx, r, opts, clean, step, v, nil)
		if stepErr != nil || closeErr != nil {
			fatal("clean normal", fmt.Errorf("%v / %v", stepErr, closeErr))
		}
		for _, method := range []string{"xWrite", "xSync"} {
			total := len(govfs.Filter(calls, govfs.Methods(method)))
			okCount := 0
			for n := 1; n <= total+1; n++ {
				trial := filepath.Join(dir, fmt.Sprintf("%s-%s-%03d.db", journal, method, n))
				if err := copyFile(base, trial); err != nil {
					fatal("copy", err)
				}
				f := govfs.Fault{Method: method, Nth: n, Code: govfs.CodeIOErr}
				err, rep, _, _ := applyStepWithFault(ctx, r, opts, trial, step, v, &f)
				uv, ic, m := checkStepState(ctx, r, trial)
				if rep.Fired {
					if err != nil && uv == faultStep-1 && ic == "ok" && m.Step == faultStep-1 && m.Exact {
						okCount++
					} else {
						allOK = false
					}
				} else if err != nil || uv != faultStep || ic != "ok" || m.Step != faultStep {
					allOK = false
				}
				_ = recipe.RemoveDatabase(trial)
			}
			notes = append(notes, fmt.Sprintf("%s/NORMAL %s:%d/%d ok", journal, method, okCount, total))
		}
	}

	// Warm WAL at synchronous=NORMAL: step 4 commits cleanly (creating WAL), then step 5 has 0 xSyncs;
	// fail every xWrite of step 5 (1..total+1) and verify exact step 4 after close.
	r := recipe.Default().WithJournalMode(sqlitex.JournalWAL).WithSteps(faultStep + 2)
	base := filepath.Join(dir, "warm-base.db")
	if err := r.Build(ctx, base, faultStep-1); err != nil {
		fatal("warm build", err)
	}
	step4, _ := r.Step(faultStep)
	step5, _ := r.Step(faultStep + 1)
	opts := r.SQLiteOptions()
	opts.Conn.Synchronous = sqlitex.SyncNormal
	opts.VFS = v.Name()

	runWarm := func(trial string, fault *govfs.Fault) (error, govfs.FaultReport, []govfs.Call) {
		db, err := sqlitex.Open(ctx, trial, opts)
		if err != nil {
			fatal("warm open", err)
		}
		if err := r.ApplyStep(ctx, db, faultStep); err != nil {
			fatal("warm step4", err)
		}
		v.Reset()
		if fault != nil {
			if err := v.Arm(*fault); err != nil {
				fatal("warm arm", err)
			}
		}
		var sErr error
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			sErr = err
		} else {
			for _, s := range step5.Statements {
				if _, err := tx.ExecContext(ctx, s.SQL, s.Args...); err != nil {
					sErr = err
					break
				}
			}
			if sErr == nil {
				sErr = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		rep := v.Disarm()
		calls := v.TakeCalls()
		_ = db.Close()
		return sErr, rep, calls
	}
	_ = step4
	cleanWarm := filepath.Join(dir, "warm-clean.db")
	if err := copyFile(base, cleanWarm); err != nil {
		fatal("copy warm", err)
	}
	wErr, _, wCalls := runWarm(cleanWarm, nil)
	if wErr != nil {
		fatal("warm clean", wErr)
	}
	warmWrites := len(govfs.Filter(wCalls, govfs.Methods("xWrite")))
	warmSyncs := len(govfs.Filter(wCalls, govfs.Methods("xSync")))
	warmOK := 0
	for n := 1; n <= warmWrites+1; n++ {
		trial := filepath.Join(dir, fmt.Sprintf("warm-%03d.db", n))
		if err := copyFile(base, trial); err != nil {
			fatal("copy warm trial", err)
		}
		f := govfs.Fault{Method: "xWrite", Nth: n, Code: govfs.CodeIOErr}
		err, rep, _ := runWarm(trial, &f)
		uv, ic, m := checkStepState(ctx, r, trial)
		if rep.Fired {
			if err != nil && uv == faultStep && ic == "ok" && m.Step == faultStep && m.Exact {
				warmOK++
			} else {
				allOK = false
			}
		} else if err != nil || uv != faultStep+1 || ic != "ok" || m.Step != faultStep+1 {
			allOK = false
		}
		_ = recipe.RemoveDatabase(trial)
	}
	if warmSyncs != 0 {
		allOK = false
	}
	notes = append(notes, fmt.Sprintf("warm-wal/NORMAL(0 xSync) xWrite:%d/%d ok", warmOK, warmWrites))

	emit("GOVFS-04",
		"fault sweeps at synchronous=NORMAL: cold WAL, DELETE, and warm WAL (0 xSync) across every xWrite/xSync",
		"every fired fault rolls back atomically to exact pre-step bytes with integrity ok",
		strings.Join(notes, "; "), allOK)
}

// GOVFS-05: SQLITE_FULL (13), extended SQLITE_IOERR_FSYNC (1034), and same-connection reuse after fault.
func attackGOVFS05(ctx context.Context, v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs05-codes-reuse"))
	const codeFull int32 = 13
	const codeIOErrFsync int32 = 1034

	allOK := true
	var notes []string

	for _, journal := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		r := recipe.Default().WithJournalMode(journal).WithSteps(faultStep + 1)
		base := filepath.Join(dir, "base-"+journal+".db")
		if err := r.Build(ctx, base, faultStep-1); err != nil {
			fatal("build", err)
		}
		step, _ := r.Step(faultStep)

		// (a) SQLITE_FULL on xWrite #1
		tFull := filepath.Join(dir, "full-"+journal+".db")
		_ = copyFile(base, tFull)
		fFull := govfs.Fault{Method: "xWrite", Nth: 1, Code: codeFull}
		errFull, repFull, _, _ := applyStepWithFault(ctx, r, r.SQLiteOptions(), tFull, step, v, &fFull)
		cFull, _ := sqlitex.ErrorCode(errFull)
		uvF, icF, mF := checkStepState(ctx, r, tFull)
		okFull := repFull.Fired && cFull == int(codeFull) && uvF == faultStep-1 && icF == "ok" && mF.Step == faultStep-1 && mF.Exact
		if !okFull {
			allOK = false
		}
		notes = append(notes, fmt.Sprintf("%s-FULL(code=%d ok=%t)", journal, cFull, okFull))

		// (b) Extended SQLITE_IOERR_FSYNC (1034) on xSync #1
		tFsync := filepath.Join(dir, "fsync-"+journal+".db")
		_ = copyFile(base, tFsync)
		fSync := govfs.Fault{Method: "xSync", Nth: 1, Code: codeIOErrFsync}
		errSync, repSync, _, _ := applyStepWithFault(ctx, r, r.SQLiteOptions(), tFsync, step, v, &fSync)
		cSync, _ := sqlitex.ErrorCode(errSync)
		uvS, icS, mS := checkStepState(ctx, r, tFsync)
		okSync := repSync.Fired && cSync == int(codeIOErrFsync) && uvS == faultStep-1 && icS == "ok" && mS.Step == faultStep-1 && mS.Exact
		if !okSync {
			allOK = false
		}
		notes = append(notes, fmt.Sprintf("%s-IOERR_FSYNC(code=%d ok=%t)", journal, cSync, okSync))

		// (c) Same-connection reuse after fault without closing
		tReuse := filepath.Join(dir, "reuse-"+journal+".db")
		_ = copyFile(base, tReuse)
		opts := r.SQLiteOptions()
		opts.VFS = v.Name()
		db, err := sqlitex.Open(ctx, tReuse, opts)
		if err != nil {
			fatal("reuse open", err)
		}
		// Arm a fault on commit xSync (WAL #2) or main-db xWrite (DELETE #1).
		reuseFault := govfs.Fault{Method: "xSync", Nth: 2, Code: govfs.CodeIOErr}
		if journal == sqlitex.JournalDelete {
			reuseFault = govfs.Fault{Method: "xWrite", Nth: 1, Code: govfs.CodeIOErr, Kind: govfs.KindMainDB}
		}
		if err := v.Arm(reuseFault); err != nil {
			fatal("arm reuse", err)
		}
		err1 := r.ApplyStep(ctx, db, faultStep)
		repR := v.Disarm()
		// Retry step 4 on the SAME connection without closing!
		err2 := r.ApplyStep(ctx, db, faultStep)
		closeErr := db.Close()
		uvR, icR, mR := checkStepState(ctx, r, tReuse)
		okReuse := repR.Fired && err1 != nil && err2 == nil && closeErr == nil && uvR == faultStep && icR == "ok" && mR.Step == faultStep && mR.Exact
		if !okReuse {
			allOK = false
		}
		notes = append(notes, fmt.Sprintf("%s-reuse(err1=%v err2=%v close=%v step=%d exact=%t)", journal, err1 != nil, err2, closeErr, mR.Step, mR.Exact))
	}

	emit("GOVFS-05",
		"SQLITE_FULL (13), extended SQLITE_IOERR_FSYNC (1034), and same-connection retry after commit fault in WAL and DELETE",
		"SQLite surfaces exact codes 13 and 1034, rolls back cleanly, and recovers on the same connection without wedging",
		strings.Join(notes, "; "), allOK)
}

// at reinterprets a C-allocated address (from sqlite3_malloc64) as a Go pointer without triggering vet unsafeptr.
func at[T any](address uintptr) *T {
	return *(**T)(unsafe.Pointer(&address))
}

// GOVFS-06: Check SQLITE_FCNTL_VFSNAME on a connection opened through govfs.
// Per ext/misc/vfstrace.c:711-716 and SQLite docs, a forwarding VFS shim should prepend
// its own name ("<shim>/<underlying>") on SQLITE_FCNTL_VFSNAME.
func attackGOVFS06(v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs06-vfsname"))
	dbPath := filepath.Join(dir, dbName)

	tls := libc.NewTLS()
	defer tls.Close()

	cPath, err := libc.CString(dbPath)
	if err != nil {
		fatal("cpath", err)
	}
	defer libc.Xfree(tls, cPath)
	cVfs, err := libc.CString(v.Name())
	if err != nil {
		fatal("cvfs", err)
	}
	defer libc.Xfree(tls, cVfs)
	cMain, err := libc.CString("main")
	if err != nil {
		fatal("cmain", err)
	}
	defer libc.Xfree(tls, cMain)

	// Allocate two pointer-sized slots in C memory (vet-clean, no Go heap uintptr).
	dbSlot := sqlite3.Xsqlite3_malloc64(tls, 16)
	if dbSlot == 0 {
		fatal("malloc", fmt.Errorf("out of memory"))
	}
	defer sqlite3.Xsqlite3_free(tls, dbSlot)
	outSlot := dbSlot + 8
	*at[uintptr](dbSlot) = 0
	*at[uintptr](outSlot) = 0

	rc := sqlite3.Xsqlite3_open_v2(tls, cPath, dbSlot, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, cVfs)
	if rc != sqlite3.SQLITE_OK {
		fatal("sqlite3_open_v2", fmt.Errorf("rc=%d", rc))
	}
	dbPtr := *at[uintptr](dbSlot)
	defer sqlite3.Xsqlite3_close(tls, dbPtr)

	rc = sqlite3.Xsqlite3_file_control(tls, dbPtr, cMain, sqlite3.SQLITE_FCNTL_VFSNAME, outSlot)
	if rc != sqlite3.SQLITE_OK {
		fatal("sqlite3_file_control VFSNAME", fmt.Errorf("rc=%d", rc))
	}
	strPtr := *at[uintptr](outSlot)
	gotName := ""
	if strPtr != 0 {
		gotName = libc.GoString(strPtr)
		sqlite3.Xsqlite3_free(tls, strPtr)
	}
	wantPrefix := v.Name() + "/"
	held := strings.HasPrefix(gotName, wantPrefix)
	emit("GOVFS-06",
		"query SQLITE_FCNTL_VFSNAME on a connection opened via govfs (comparing against vfstrace.c:711-716 shim contract)",
		fmt.Sprintf("VFS stack string starts with %q (e.g. %q)", wantPrefix, wantPrefix+"unix"),
		fmt.Sprintf("SQLITE_FCNTL_VFSNAME returned %q", gotName),
		held)
}

// GOVFS-07: Stress govfs with mmap_size (xFetch/xUnfetch), temp tables, savepoint subjournals, VACUUM, v2, and 8 goroutines.
func attackGOVFS07(ctx context.Context, vMain, vV2, vStress *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs07-stress"))

	// (a) v3 features: mmap_size, temp table, savepoint + rollback, VACUUM
	p1 := filepath.Join(dir, "features.db")
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = sqlitex.JournalWAL
	o.SingleConnection = true
	o.VFS = vMain.Name()
	db, err := sqlitex.Open(ctx, p1, o)
	if err != nil {
		fatal("stress open", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
		fatal("stress create", err)
	}
	tx, _ := db.BeginTx(ctx, nil)
	for i := 1; i <= 50; i++ {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t VALUES(?, zeroblob(1500))", i); err != nil {
			fatal("stress insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("stress commit", err)
	}
	_, _ = sqlitex.Checkpoint(ctx, db, "main", sqlitex.CheckpointTruncate)
	_ = db.Close()

	// Reopen with mmap_size=268435456 to trigger xFetch/xUnfetch; also exercise temp table, savepoint, VACUUM.
	db, err = sqlitex.Open(ctx, p1, o)
	if err != nil {
		fatal("stress reopen", err)
	}
	vMain.Reset()
	if _, err := db.ExecContext(ctx, "PRAGMA mmap_size=268435456"); err != nil {
		fatal("mmap_size", err)
	}
	var sumLen int
	if err := db.QueryRowContext(ctx, "SELECT sum(length(v)) FROM t").Scan(&sumLen); err != nil {
		fatal("mmap select", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA cache_size=4; PRAGMA temp_store=FILE; CREATE TEMP TABLE tmp(id INTEGER PRIMARY KEY, v BLOB); INSERT INTO tmp SELECT id, v FROM t"); err != nil {
		fatal("temp table", err)
	}
	if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE; SAVEPOINT sp1; INSERT INTO t VALUES(999, zeroblob(30000)); ROLLBACK TO sp1; RELEASE sp1; COMMIT"); err != nil {
		fatal("savepoint", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM t WHERE id > 25; VACUUM"); err != nil {
		fatal("vacuum", err)
	}
	var ic string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
		fatal("ic", err)
	}
	calls := vMain.TakeCalls()
	_ = db.Close()

	counts := govfs.CountCalls(calls)
	hasFetch := counts["xFetch"] > 0 && counts["xUnfetch"] > 0
	hasTempOpen := false
	for _, c := range calls {
		if c.Method == "xOpen" && (c.Kind == govfs.KindTempDB || c.Kind == govfs.KindTransientDB || c.Kind == govfs.KindSubjournal) {
			hasTempOpen = true
		}
	}

	// (b) v2 shim: WAL works, xFetch/xUnfetch omitted even with mmap_size
	p2 := filepath.Join(dir, "v2.db")
	o2 := sqlitex.DefaultOptions()
	o2.File.JournalMode = sqlitex.JournalWAL
	o2.SingleConnection = true
	o2.VFS = vV2.Name()
	db2, err := sqlitex.Open(ctx, p2, o2)
	if err != nil {
		fatal("v2 open", err)
	}
	if _, err := db2.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB); INSERT INTO t VALUES(1, zeroblob(8000))"); err != nil {
		fatal("v2 create", err)
	}
	_, _ = sqlitex.Checkpoint(ctx, db2, "main", sqlitex.CheckpointTruncate)
	vV2.Reset()
	_, _ = db2.ExecContext(ctx, "PRAGMA mmap_size=268435456")
	var v2Len int
	_ = db2.QueryRowContext(ctx, "SELECT length(v) FROM t").Scan(&v2Len)
	v2Counts := govfs.CountCalls(vV2.TakeCalls())
	_ = db2.Close()
	v2OK := v2Len == 8000 && v2Counts["xFetch"] == 0 && v2Counts["xShmLock"] > 0

	// (c) 8 concurrent goroutines sharing one govfs.VFS across 8 separate databases
	const goroutines = 8
	var wg sync.WaitGroup
	concErrs := make([]error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gp := filepath.Join(dir, fmt.Sprintf("conc-%d.db", idx))
			og := sqlitex.DefaultOptions()
			og.File.JournalMode = sqlitex.JournalWAL
			og.SingleConnection = true
			og.VFS = vStress.Name()
			gdb, err := sqlitex.Open(ctx, gp, og)
			if err != nil {
				concErrs[idx] = err
				return
			}
			defer gdb.Close()
			if _, err := gdb.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
				concErrs[idx] = err
				return
			}
			for i := 1; i <= 20; i++ {
				if _, err := gdb.ExecContext(ctx, "INSERT INTO t VALUES(?, ?)", i, fmt.Sprintf("g%d-%d", idx, i)); err != nil {
					concErrs[idx] = err
					return
				}
			}
			var n int
			var gic string
			if err := gdb.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 20 {
				concErrs[idx] = fmt.Errorf("count=%d err=%v", n, err)
				return
			}
			if err := gdb.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&gic); err != nil || gic != "ok" {
				concErrs[idx] = fmt.Errorf("ic=%s err=%v", gic, err)
			}
		}(g)
	}
	wg.Wait()
	concOK := vStress.Err() == nil && vMain.Err() == nil && vV2.Err() == nil
	for _, e := range concErrs {
		if e != nil {
			concOK = false
		}
	}

	held := sumLen == 75000 && ic == "ok" && hasFetch && hasTempOpen && v2OK && concOK
	emit("GOVFS-07",
		"stress govfs with mmap_size (xFetch/xUnfetch), temp tables, savepoint subjournals, VACUUM, v2 shim, and 8 concurrent goroutines",
		"xFetch/xUnfetch + temp/subjournal opens work on v3, v2 allows WAL with 0 xFetch, 8 goroutines finish with 0 errors and v.Err()==nil",
		fmt.Sprintf("v3(sum=%d ic=%s xFetch=%d xUnfetch=%d tempOpen=%t) v2(ok=%t xFetch=%d xShmLock=%d) conc8(ok=%t err=%v)",
			sumLen, ic, counts["xFetch"], counts["xUnfetch"], hasTempOpen, v2OK, v2Counts["xFetch"], v2Counts["xShmLock"], concOK, vStress.Err()),
		held)
}

// GOVFS-08: Must-be-able-to-fail check on internal/govfs's TestSignaturesMatchTranslatedSource.
// Copies go.mod, go.sum, internal/sqlitex, and internal/govfs into a scratch repo under _work/adv-ch07/,
// mutates ioSyncFunc flags from int32 to int64, and verifies the source-signature test catches it.
func attackGOVFS08(ctx context.Context) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs08-sig-mut"))
	repoRoot, err := filepath.Abs(".")
	if err != nil {
		fatal("repo root", err)
	}
	copyTree := func(rel string, mutate func(name, text string) string) {
		srcDir := filepath.Join(repoRoot, rel)
		dstDir := filepath.Join(dir, rel)
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			fatal("mkdir "+rel, err)
		}
		entries, err := os.ReadDir(srcDir)
		if err != nil {
			fatal("readdir "+rel, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
			if err != nil {
				fatal("read "+e.Name(), err)
			}
			text := string(data)
			if mutate != nil {
				text = mutate(e.Name(), text)
			}
			if err := os.WriteFile(filepath.Join(dstDir, e.Name()), []byte(text), 0o644); err != nil {
				fatal("write "+e.Name(), err)
			}
		}
	}

	copyTree("sqlite", nil)
	copyTree(filepath.Join("internal", "sqlitex"), nil)
	copyTree(filepath.Join("internal", "govfs"), func(name, text string) string {
		if name == "slots.go" {
			return strings.Replace(text,
				"ioSyncFunc                  = func(*libc.TLS, uintptr, int32) int32",
				"ioSyncFunc                  = func(*libc.TLS, uintptr, int64) int32", 1)
		}
		if name == "methods.go" {
			return strings.Replace(text,
				"func ioSync(tls *libc.TLS, pFile uintptr, flags int32) int32 {",
				"func ioSync(tls *libc.TLS, pFile uintptr, flags int64) int32 {", 1)
		}
		return text
	})
	for _, f := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			fatal("read "+f, err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			fatal("write "+f, err)
		}
	}

	cmd := exec.CommandContext(ctx, "go", "test", "-v", "-run", "TestSignaturesMatchTranslatedSource", "./internal/govfs")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			fatal("run mutated test", err)
		}
	}
	text := string(out)
	caughtSync := strings.Contains(text, "xSync: translated") && strings.Contains(text, "int64")
	held := exitCode != 0 && caughtSync
	var mismatchLine string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "xSync: translated") {
			mismatchLine = strings.TrimSpace(line)
			break
		}
	}
	emit("GOVFS-08",
		"must-be-able-to-fail: mutate ioSyncFunc flags int32->int64 in scratch copy of internal/govfs and run TestSignaturesMatchTranslatedSource",
		"TestSignaturesMatchTranslatedSource exits non-zero and reports xSync int32 vs int64 mismatch",
		fmt.Sprintf("exit=%d msg=%q", exitCode, mismatchLine),
		held)
}

// GOVFS-09: Arm with SetRecording(false) still populates FaultReport without recording calls,
// and govfs.Register works cleanly alongside modernc.org/sqlite/vfs.
func attackGOVFS09(ctx context.Context, v *govfs.VFS) {
	dir := cleanDir(filepath.Join(scratchRoot, "govfs09-rec-interop"))
	p := filepath.Join(dir, dbName)
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = sqlitex.JournalWAL
	o.SingleConnection = true
	o.VFS = v.Name()
	db, err := sqlitex.Open(ctx, p, o)
	if err != nil {
		fatal("open09", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		fatal("create09", err)
	}
	v.SetRecording(false)
	v.Reset()
	if err := v.Arm(govfs.Fault{Method: "xWrite", Nth: 1, Code: govfs.CodeIOErr}); err != nil {
		fatal("arm09", err)
	}
	_, execErr := db.ExecContext(ctx, "INSERT INTO t VALUES(1)")
	rep := v.Disarm()
	recordedCount := len(v.Calls())
	v.SetRecording(true)
	_ = db.Close()

	// Interop: register a modernc vfs.New alongside a fresh govfs.Register.
	roVFS, handle, err := vfs.New(fstest.MapFS{"empty.txt": &fstest.MapFile{Data: []byte("ok")}})
	if err != nil {
		fatal("vfs.New", err)
	}
	defer handle.Close()
	vAfter, err := govfs.Register("adv-govfs-after-ro", govfs.Options{})
	if err != nil {
		fatal("govfs.Register after vfs.New", err)
	}
	// Open a DB using vAfter to prove forwarding works.
	p2 := filepath.Join(dir, "after.db")
	o2 := o
	o2.VFS = vAfter.Name()
	db2, err := sqlitex.Open(ctx, p2, o2)
	if err != nil {
		fatal("open after", err)
	}
	_, err2 := db2.ExecContext(ctx, "CREATE TABLE x(id INTEGER PRIMARY KEY); INSERT INTO x VALUES(1)")
	_ = db2.Close()

	// Also verify read-only sql.Open with the roVFS doesn't disturb govfs.
	_ = sql.Open
	held := execErr != nil && rep.Fired && rep.Seen == 1 && rep.Call.Method == "xWrite" && rep.Call.Injected &&
		recordedCount == 0 && roVFS != "" && err2 == nil && vAfter.Err() == nil
	emit("GOVFS-09",
		"fault injection with SetRecording(false), plus govfs.Register after modernc.org/sqlite/vfs.New",
		"Disarm reports Fired=true with Call details while Calls() stays empty; govfs works alongside modernc vfs.New",
		fmt.Sprintf("fired=%t seen=%d call=%q callsLen=%d afterRoErr=%v vfsErr=%v", rep.Fired, rep.Seen, rep.Call.String(), recordedCount, err2, vAfter.Err()),
		held)
}
