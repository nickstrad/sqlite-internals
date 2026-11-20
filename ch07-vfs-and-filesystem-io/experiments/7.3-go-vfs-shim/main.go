// Experiment 7.3 registers the pure-Go forwarding VFS shim (internal/govfs) on
// top of modernc.org/sqlite's default VFS. Part A compares the Go driver's
// per-commit VFS call sequence call-for-call against the C CLI's -vfstrace in
// DELETE and WAL modes at NORMAL and FULL, also recording the real SQLITE_SYNC_*
// flag values. Part B fails every Nth xWrite and xSync of a recipe step with
// SQLITE_IOERR in both journal modes and verifies SQLite rolls back cleanly to
// the exact pre-step database bytes.
//
// Run from the repo root (requires the pinned CLI via go run ./cmd/course build-tools):
//
//	go run ./ch07-vfs-and-filesystem-io/experiments/7.3-go-vfs-shim
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/govfs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const (
	vfsName    = "course-govfs"
	dbName     = "t.db"
	setupSQL   = "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t(id, v) VALUES(1, 'one')"
	measureSQL = "INSERT INTO t(id, v) VALUES(2, 'two')"
	faultStep  = 4
)

type config struct {
	journal string
	sync    string
}

func (c config) String() string {
	return c.journal + "/" + c.sync
}

var configs = []config{
	{sqlitex.JournalDelete, sqlitex.SyncNormal},
	{sqlitex.JournalDelete, sqlitex.SyncFull},
	{sqlitex.JournalWAL, sqlitex.SyncNormal},
	{sqlitex.JournalWAL, sqlitex.SyncFull},
}

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

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("7.3")
	fmt.Printf("INFO: work dir %s\n", sqlitex.DisplayPath(dir))
	cli := sqlitex.MustLocateCLI(ctx)
	v, err := govfs.Register(vfsName, govfs.Options{})
	if err != nil {
		fatal("register", err)
	}
	fmt.Printf("registered Go VFS %q (io_methods version %d) forwarding to the default VFS; self-check passed\n", v.Name(), v.IOVersion())

	fmt.Println()
	fmt.Println("== part A: VFS calls of one committed INSERT, Go driver through the Go VFS vs the C CLI's -vfstrace ==")
	fmt.Printf("setup (not measured): %s\nmeasured: %s\n", setupSQL, measureSQL)
	table := map[config]govfs.Counts{}
	for _, c := range configs {
		table[c] = partA(ctx, cli, v, dir, c)
	}
	printCountTable(table)

	fmt.Println()
	fmt.Println("== part B: fail the N-th xWrite or xSync of one recipe commit with SQLITE_IOERR, for every N ==")
	for _, journal := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		partB(ctx, v, dir, journal)
	}
	if err := v.Err(); err != nil {
		check(false, "the Go VFS reported no internal error: "+err.Error())
	}
	if len(failures) > 0 {
		fmt.Printf("FAIL: %d claim(s) did not hold\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("ALL CHECKS PASSED")
}

func goOptions(c config) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = c.journal
	o.Conn.Synchronous = c.sync
	o.SingleConnection = true
	o.VFS = vfsName
	return o
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

// goKey normalizes a recorded govfs.Call to the same comparison tuple produced
// by cliKey for CLI -vfstrace output, including vfstrace's sync flag label
// (vfstrace.c:453-458).
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

func runCLI(ctx context.Context, cli sqlitex.CLI, dir string, c config) ([]trace.VFSCall, error) {
	path := filepath.Join(dir, "cli-"+strings.ToLower(c.journal+"-"+c.sync)+".db")
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	script := cliPrelude(goOptions(c)) + setupSQL + ";\n.print MARK-BEGIN\n" + measureSQL + ";\n.print MARK-END\n"
	result, err := cli.Run(ctx, sqlitex.CLIRun{Args: []string{"-vfstrace"}, Stdin: strings.NewReader(sqlitex.OpenScript(path, script)), Dir: dir})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("cli exit %d: %s", result.ExitCode, result.Stderr)
	}
	var calls []trace.VFSCall
	inside := false
	for line := range strings.SplitSeq(result.Stdout, "\n") {
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
		call, ok := trace.ParseVFSLine(line)
		if ok {
			calls = append(calls, call)
		}
	}
	return trace.RelativeVFS(dir, calls), nil
}

func runGo(ctx context.Context, v *govfs.VFS, dir string, c config) ([]govfs.Call, error) {
	path := filepath.Join(dir, "go-"+strings.ToLower(c.journal+"-"+c.sync)+".db")
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	db, err := sqlitex.Open(ctx, path, goOptions(c))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, setupSQL); err != nil {
		return nil, err
	}
	v.Reset()
	if _, err := db.ExecContext(ctx, measureSQL); err != nil {
		return nil, err
	}
	calls := v.TakeCalls()
	return calls, nil
}

func renameFiles(calls []govfs.Call, from, to string) []govfs.Call {
	out := make([]govfs.Call, len(calls))
	for i, c := range calls {
		c.File = strings.Replace(c.File, from, to, 1)
		out[i] = c
	}
	return out
}

func partA(ctx context.Context, cli sqlitex.CLI, v *govfs.VFS, dir string, c config) govfs.Counts {
	goCalls, err := runGo(ctx, v, dir, c)
	if err != nil {
		fatal("go "+c.String(), err)
	}
	cliCalls, err := runCLI(ctx, cli, dir, c)
	if err != nil {
		fatal("cli "+c.String(), err)
	}
	name := "go-" + strings.ToLower(c.journal+"-"+c.sync) + ".db"
	goCalls = renameFiles(goCalls, name, dbName)
	cliName := "cli-" + strings.ToLower(c.journal+"-"+c.sync) + ".db"
	fmt.Println()
	fmt.Printf("-- journal_mode=%s synchronous=%s: %d VFS calls through the Go VFS (file shown as %s) --\n", c.journal, c.sync, len(goCalls), dbName)
	for _, call := range goCalls {
		line := call.String()
		if call.Method == "xSync" {
			line += fmt.Sprintf("   [vfstrace would print %s]", govfs.VFSTraceSyncLabel(call.Flags))
		}
		fmt.Printf("  %3d %s\n", call.Seq, line)
	}
	goKeys := make([]string, len(goCalls))
	for i, call := range goCalls {
		goKeys[i] = goKey(call)
	}
	cliKeys := make([]string, len(cliCalls))
	for i, call := range cliCalls {
		cliKeys[i] = strings.Replace(cliKey(call), cliName, dbName, 1)
	}
	same := len(goKeys) == len(cliKeys)
	first := -1
	for i := range min(len(goKeys), len(cliKeys)) {
		if goKeys[i] != cliKeys[i] {
			same = false
			first = i
			break
		}
	}
	goCounts := govfs.CountCalls(goCalls)
	cliCounts := govfs.Counts{}
	for _, call := range cliCalls {
		cliCounts[call.Method]++
	}
	fmt.Printf("  Go VFS counts:        %s\n", goCounts)
	fmt.Printf("  CLI -vfstrace counts: %s\n", cliCounts)
	if !same {
		if first < 0 {
			first = min(len(goKeys), len(cliKeys))
		}
		at := func(list []string, i int) string {
			if i < len(list) {
				return list[i]
			}
			return "(none)"
		}
		fmt.Printf("  first difference at call %d: Go %q, CLI %q\n", first+1, at(goKeys, first), at(cliKeys, first))
	}
	check(same && len(goCalls) > 0 && goCounts.String() == cliCounts.String(), fmt.Sprintf("%s: the Go driver makes the same %d VFS calls, in the same order with the same files, offsets, lengths, lock levels and sync labels, as the C CLI", c, len(goCalls)))
	var flags []string
	for _, call := range goCalls {
		if call.Method == "xSync" {
			flags = append(flags, fmt.Sprintf("%s=0x%x(%s)", call.File, call.Flags, govfs.SyncFlagsString(call.Flags)))
		}
	}
	if len(flags) > 0 {
		fmt.Printf("  xSync flags SQLite passed: %s\n", strings.Join(flags, " "))
	}
	return goCounts
}

func printCountTable(table map[config]govfs.Counts) {
	methods := map[string]bool{}
	for _, counts := range table {
		for m := range counts {
			methods[m] = true
		}
	}
	names := make([]string, 0, len(methods))
	for m := range methods {
		names = append(names, m)
	}
	sort.Strings(names)
	fmt.Println()
	fmt.Println("per-commit VFS calls, one committed single-row INSERT (Go driver = CLI where the checks above passed):")
	fmt.Printf("  %-24s", "method")
	for _, c := range configs {
		fmt.Printf(" %14s", c)
	}
	fmt.Println()
	for _, m := range names {
		fmt.Printf("  %-24s", m)
		for _, c := range configs {
			fmt.Printf(" %14d", table[c][m])
		}
		fmt.Println()
	}
	fmt.Printf("  %-24s", "total")
	for _, c := range configs {
		total := 0
		for _, n := range table[c] {
			total += n
		}
		fmt.Printf(" %14d", total)
	}
	fmt.Println()
}

type outcome struct {
	stepErr     string
	stepCode    int
	where       string
	closeErr    string
	userVersion int
	rowPresent  bool
	integrity   string
	match       recipe.Match
}

func (o outcome) rolledBack() bool {
	return o.stepCode == int(govfs.CodeIOErr) && o.closeErr == "" && !o.rowPresent && o.integrity == "ok" && o.match.Step == faultStep-1 && o.match.Exact && o.userVersion == faultStep-1
}

func (o outcome) committed() bool {
	return o.stepErr == "" && o.closeErr == "" && o.rowPresent && o.match.Step == faultStep && o.match.Exact
}

func trialHeld(n, total int, fired bool, o outcome) bool {
	if n <= total {
		return fired && o.rolledBack()
	}
	return !fired && o.committed()
}

func (o outcome) String() string {
	stmt := "step committed"
	if o.stepErr != "" {
		stmt = fmt.Sprintf("error at %s: %s", o.where, o.stepErr)
	}
	row := "absent"
	if o.rowPresent {
		row = "present"
	}
	text := fmt.Sprintf("%s | reopen: user_version=%d, marker row %s, integrity %s, file = %s", stmt, o.userVersion, row, o.integrity, o.match)
	if o.closeErr != "" {
		text += " | close: " + o.closeErr
	}
	return text
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
		out.Close()
		return err
	}
	return out.Close()
}

func errorText(err error) string {
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("SQLite error code %d (%s)", code, strings.TrimSpace(err.Error()))
	}
	return err.Error()
}

// applyThroughVFS runs one recipe step inside a single transaction through the
// shim VFS with an optional armed fault, disarms and drains recorded calls, and
// closes the connection before returning so the caller can safely reopen and
// inspect the file.
func applyThroughVFS(ctx context.Context, r recipe.Recipe, path string, step recipe.Step, v *govfs.VFS, fault *govfs.Fault) (stepErr error, where string, report govfs.FaultReport, calls []govfs.Call, closeErr error) {
	o := r.SQLiteOptions()
	o.VFS = vfsName
	db, err := sqlitex.Open(ctx, path, o)
	if err != nil {
		fatal("open through the Go VFS", err)
	}
	v.Reset()
	if fault != nil {
		if err := v.Arm(*fault); err != nil {
			fatal("arm", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		stepErr, where = err, "BEGIN"
	} else {
		for i, s := range step.Statements {
			if _, err := tx.ExecContext(ctx, s.SQL, s.Args...); err != nil {
				stepErr, where = err, fmt.Sprintf("statement %d/%d (%s)", i+1, len(step.Statements), firstWords(s.SQL))
				break
			}
		}
		if stepErr == nil {
			if err := tx.Commit(); err != nil {
				stepErr, where = err, "COMMIT"
			}
		} else {
			tx.Rollback()
		}
	}
	report = v.Disarm()
	calls = v.TakeCalls()
	closeErr = db.Close()
	return stepErr, where, report, calls, closeErr
}

func firstWords(sql string) string {
	if i := strings.IndexByte(sql, '('); i >= 0 {
		sql = sql[:i]
	}
	fields := strings.Fields(sql)
	if len(fields) > 3 {
		fields = fields[:3]
	}
	return strings.Join(fields, " ")
}

func markerEventID(step recipe.Step) int64 {
	for _, s := range step.Statements {
		if strings.HasPrefix(s.SQL, "INSERT INTO events") {
			switch id := s.Args[0].(type) {
			case int:
				return int64(id)
			case int64:
				return id
			}
		}
	}
	return -1
}

func inspect(ctx context.Context, r recipe.Recipe, path string, markerID int64) outcome {
	var o outcome
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("reopen", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&o.userVersion); err != nil {
		fatal("user_version", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE id = ?", markerID).Scan(&n); err != nil {
		fatal("marker row", err)
	}
	o.rowPresent = n > 0
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&o.integrity); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close after inspect", err)
	}
	match, err := r.MatchStep(ctx, path)
	if err != nil {
		fatal("match", err)
	}
	o.match = match
	return o
}

func partB(ctx context.Context, v *govfs.VFS, dir, journal string) {
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
	markerID := markerEventID(step)
	baseInfo, err := os.Stat(base)
	if err != nil {
		fatal("stat base", err)
	}
	fmt.Println()
	fmt.Printf("-- %s, synchronous=%s: recipe seed %d, the database after step %d (%d bytes, at rest) gets step %d through the Go VFS --\n", journal, r.SQLiteOptions().Conn.Synchronous, r.Seed, faultStep-1, baseInfo.Size(), faultStep)
	fmt.Printf("step %d: %s; marker row: events id %d (inserted by this step)\n", faultStep, step.Summary(), markerID)

	clean := filepath.Join(dir, "clean-"+journal+".db")
	if err := copyFile(base, clean); err != nil {
		fatal("copy", err)
	}
	stepErr, _, _, calls, closeErr := applyThroughVFS(ctx, r, clean, step, v, nil)
	if stepErr != nil || closeErr != nil {
		fatal("clean run", fmt.Errorf("%v / %v", stepErr, closeErr))
	}
	cleanOutcome := inspect(ctx, r, clean, markerID)
	writes := govfs.Filter(calls, govfs.Methods("xWrite"))
	syncs := govfs.Filter(calls, govfs.Methods("xSync"))
	byFile := map[string][2]int64{}
	for _, w := range writes {
		agg := byFile[w.File]
		agg[0]++
		agg[1] += w.Amount
		byFile[w.File] = agg
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	fmt.Printf("clean run: %d xWrite, %d xSync; %s\n", len(writes), len(syncs), cleanOutcome)
	for _, f := range files {
		fmt.Printf("  xWrite to %s: %d calls, %d bytes\n", strings.Replace(f, "clean-"+journal+".db", dbName, 1), byFile[f][0], byFile[f][1])
	}
	for _, s := range syncs {
		fmt.Printf("  %s (call %d of %d in the step)\n", strings.Replace(s.String(), "clean-"+journal+".db", dbName, 1), s.Seq, len(calls))
	}
	check(cleanOutcome.match.Step == faultStep && cleanOutcome.match.Exact && cleanOutcome.rowPresent, fmt.Sprintf("%s: without a fault the step commits and the file equals recipe step %d exactly", journal, faultStep))

	for _, method := range []string{"xWrite", "xSync"} {
		total := len(govfs.Filter(calls, govfs.Methods(method)))
		allRolledBack := true
		var summary []string
		for n := 1; n <= total+1; n++ {
			trial := filepath.Join(dir, fmt.Sprintf("fault-%s-%s-%03d.db", journal, method, n))
			if err := copyFile(base, trial); err != nil {
				fatal("copy", err)
			}
			fault := govfs.Fault{Method: method, Nth: n, Code: govfs.CodeIOErr}
			stepErr, where, report, _, closeErr := applyThroughVFS(ctx, r, trial, step, v, &fault)
			o := inspect(ctx, r, trial, markerID)
			if stepErr != nil {
				o.stepErr, o.where = errorText(stepErr), where
				o.stepCode, _ = sqlitex.PrimaryErrorCode(stepErr)
			}
			if closeErr != nil {
				o.closeErr = errorText(closeErr)
			}
			target := "never reached (the step has fewer calls)"
			if report.Fired {
				target = strings.Replace(report.Call.String(), filepath.Base(trial), dbName, 1)
			}
			fmt.Printf("  %s #%d -> %s\n      %s\n", method, n, target, o)
			if n <= total {
				if !trialHeld(n, total, report.Fired, o) {
					allRolledBack = false
					summary = append(summary, fmt.Sprintf("%s #%d", method, n))
				}
			} else {
				check(trialHeld(n, total, report.Fired, o), fmt.Sprintf("%s: with %s #%d never reached, the step commits normally", journal, method, n))
			}
			if err := recipe.RemoveDatabase(trial); err != nil {
				fatal("remove", err)
			}
		}
		claim := fmt.Sprintf("%s: for each of the %d %s calls of the commit, failing it with SQLITE_IOERR makes the step fail, the marker row is absent after reopen, integrity_check is ok and the file equals the pre-step recipe state (step %d)", journal, total, method, faultStep-1)
		if !allRolledBack {
			claim += "; exceptions: " + strings.Join(summary, ", ")
		}
		check(allRolledBack && total > 0, claim)
	}
}
