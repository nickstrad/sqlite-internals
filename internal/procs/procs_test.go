package procs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

var heldLock *os.File

var testRoles = map[string]Role{
	RoleSQL:    SQLRole,
	RoleCopier: CopierRole,
	"echo": func(ctx context.Context, args []string) (*Actor, error) {
		return &Actor{Actions: map[string]Handler{
			"echo": func(ctx context.Context, args []string) (string, error) { return strings.Join(args, "|"), nil },
			"fail": func(ctx context.Context, args []string) (string, error) { return "", errors.New("asked to fail") },
			"crash": func(ctx context.Context, args []string) (string, error) {
				os.Exit(7)
				return "", nil
			},
			"sleep": func(ctx context.Context, args []string) (string, error) {
				d, err := time.ParseDuration(args[0])
				if err != nil {
					return "", err
				}
				time.Sleep(d)
				return "slept", nil
			},
			"lie": func(ctx context.Context, args []string) (string, error) {
				line, _ := json.Marshal(Result{Step: 999, Action: "other"})
				os.NewFile(replyFD, "replies").Write(append(line, '\n'))
				return "", nil
			},
			"lock": func(ctx context.Context, args []string) (string, error) {
				file, err := os.OpenFile(args[0], os.O_RDWR, 0)
				if err != nil {
					return "", err
				}
				heldLock = file
				lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 120, Len: 1}
				return "", syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock)
			},
			"stray-close": func(ctx context.Context, args []string) (string, error) {
				file, err := os.Open(args[0])
				if err != nil {
					return "", err
				}
				return "", file.Close()
			},
		}, Close: func() error {
			if len(args) > 0 && args[0] == "close-fails" {
				return errors.New("close failed on purpose")
			}
			return nil
		}}, nil
	},
	"refuse": func(ctx context.Context, args []string) (*Actor, error) {
		return nil, errors.New("this role never starts")
	},
}

func TestMain(m *testing.M) {
	if Serve(testRoles) {
		return
	}
	os.Exit(m.Run())
}

func TestResultCodeNames(t *testing.T) {
	cases := map[int]string{0: "SQLITE_OK", 5: "SQLITE_BUSY", 517: "SQLITE_BUSY_SNAPSHOT", 261: "SQLITE_BUSY_RECOVERY", 15: "SQLITE_PROTOCOL", 5898: "SQLITE_IOERR_DELETE_NOENT", 1290: "SQLITE_IOERR+extended(5)", 9999: "SQLITE_PROTOCOL+extended(39)", 200: "SQLITE_code_200"}
	for code, want := range cases {
		if got := sqlitex.ResultCodeName(code); got != want {
			t.Errorf("ResultCodeName(%d) = %q, want %q", code, got, want)
		}
	}
	if got := (Result{Code: 517, Error: "x"}).Outcome(); got != "SQLITE_BUSY_SNAPSHOT (517)" {
		t.Errorf("Outcome = %q", got)
	}
	if got := (Result{Value: "3"}).Outcome(); got != "SQLITE_OK 3" {
		t.Errorf("Outcome = %q", got)
	}
	if got := (Result{Code: CodeNotSQLite, Error: "boom"}).Outcome(); got != "ERROR boom" {
		t.Errorf("Outcome = %q", got)
	}
}

func TestResultForNonSQLiteError(t *testing.T) {
	r := ResultFor(Request{Step: 4, Action: "a"}, "", errors.New("plain"))
	if r.Code != CodeNotSQLite || r.OK() || r.Step != 4 || r.Action != "a" {
		t.Fatalf("got %+v", r)
	}
}

func runChildInProcess(t *testing.T, role string, requests string) ([]Result, error) {
	t.Helper()
	var out bytes.Buffer
	err := RunChild(context.Background(), testRoles, role, nil, strings.NewReader(requests), &out)
	var results []Result
	decoder := json.NewDecoder(&out)
	for {
		var r Result
		if decodeErr := decoder.Decode(&r); decodeErr == io.EOF {
			break
		} else if decodeErr != nil {
			t.Fatalf("decode: %v", decodeErr)
		}
		results = append(results, r)
	}
	return results, err
}

func TestRunChildProtocol(t *testing.T) {
	results, err := runChildInProcess(t, "echo", `{"step":1,"action":"echo","args":["a","b"]}
{"step":2,"action":"fail"}
{"step":3,"action":"nope"}
{"step":-1,"action":"exit"}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 || results[0].Action != ActionReady {
		t.Fatalf("results %+v", results)
	}
	if results[1].Value != "a|b" || !results[1].OK() {
		t.Errorf("echo: %+v", results[1])
	}
	if results[2].OK() || results[2].Code != CodeNotSQLite || results[2].Error != "asked to fail" {
		t.Errorf("fail: %+v", results[2])
	}
	if results[3].OK() || !strings.Contains(results[3].Error, "no action") {
		t.Errorf("unknown action: %+v", results[3])
	}
	if results[4].Step != -1 || !results[4].OK() {
		t.Errorf("exit: %+v", results[4])
	}
}

func TestRunChildErrors(t *testing.T) {
	if _, err := runChildInProcess(t, "missing", ""); !errors.Is(err, ErrUnknownRole) {
		t.Errorf("unknown role: %v", err)
	}
	if _, err := runChildInProcess(t, "refuse", ""); err == nil {
		t.Error("a role that fails to start must be an error")
	}
	if _, err := runChildInProcess(t, "echo", "not json\n"); err == nil {
		t.Error("malformed request accepted")
	}
	if _, err := runChildInProcess(t, "echo", `{"step":1,"action":"echo"}`+"\n"); err == nil {
		t.Error("request pipe closed without exit must be an error")
	}
}

func TestChildRoundTripAndStop(t *testing.T) {
	child, err := StartChild("one", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if child.Pid <= 0 || child.Pid == os.Getpid() {
		t.Fatalf("pid %d", child.Pid)
	}
	result, err := child.Do(1, "echo", "x")
	if err != nil || result.Value != "x" {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := child.Do(0, "echo"); err == nil {
		t.Error("step 0 accepted")
	}
	if err := child.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Do(2, "echo"); !errors.Is(err, ErrStopped) {
		t.Errorf("after stop: %v", err)
	}
}

func TestChildCloseErrorIsReported(t *testing.T) {
	child, err := StartChild("closer", "echo", "close-fails")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Stop(); err == nil || !strings.Contains(err.Error(), "close failed on purpose") {
		t.Fatalf("Stop = %v", err)
	}
}

func TestChildThatDiesIsReported(t *testing.T) {
	child, err := StartChild("crasher", "echo")
	if err != nil {
		t.Fatal(err)
	}
	_, err = child.Do(1, "crash")
	if !errors.Is(err, ErrChildDied) || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("crash: %v", err)
	}
}

func TestRoleThatFailsToStart(t *testing.T) {
	_, err := StartChild("refuser", "refuse")
	if !errors.Is(err, ErrChildDied) || !strings.Contains(err.Error(), "this role never starts") {
		t.Fatalf("start: %v", err)
	}
}

func TestChildTimeoutKillsIt(t *testing.T) {
	child, err := StartChild("sleeper", "echo")
	if err != nil {
		t.Fatal(err)
	}
	child.Timeout = 200 * time.Millisecond
	_, err = child.Do(1, "sleep", "1h")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: %v", err)
	}
	select {
	case <-child.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("child still running after the timeout")
	}
}

func TestAsyncCallAndOneOutstandingRequest(t *testing.T) {
	child, err := StartChild("async", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	call, err := child.Start(1, "sleep", "300ms")
	if err != nil {
		t.Fatal(err)
	}
	if answered, err := call.Answered(); answered || err != nil {
		t.Fatalf("answered %v err %v right after sending", answered, err)
	}
	if _, err := child.Start(2, "echo"); !errors.Is(err, ErrBusyChild) {
		t.Fatalf("second request: %v", err)
	}
	result, err := call.Wait()
	if err != nil || result.Value != "slept" {
		t.Fatalf("%+v %v", result, err)
	}
	if answered, err := call.Answered(); !answered || err != nil {
		t.Fatalf("after Wait: %v %v", answered, err)
	}
	if err := child.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestStopWithOutstandingRequestFails(t *testing.T) {
	child, err := StartChild("pending", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Start(1, "sleep", "1h"); err != nil {
		t.Fatal(err)
	}
	if err := child.Stop(); !errors.Is(err, ErrBusyChild) {
		t.Fatalf("Stop = %v", err)
	}
}

func TestMismatchedReplyIsAnError(t *testing.T) {
	child, err := StartChild("liar", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	if _, err := child.Do(1, "lie"); !errors.Is(err, ErrStepMismatch) {
		t.Fatalf("lie: %v", err)
	}
}

func TestScriptNumbersStepsAndPrints(t *testing.T) {
	var out bytes.Buffer
	script := NewScript(&out)
	child, err := script.Spawn("a", "echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.Spawn("a", "echo"); err == nil {
		t.Error("duplicate child name accepted")
	}
	if _, err := script.Do(child, "echo", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := script.Do(child, "fail"); err != nil {
		t.Fatal(err)
	}
	if name, ok := script.NameOfPid(child.Pid); !ok || name != "a" {
		t.Errorf("NameOfPid = %q %v", name, ok)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	want := "step  1  a          echo 1                               -> SQLITE_OK 1\nstep  2  a          fail                                 -> ERROR asked to fail\n"
	if out.String() != want {
		t.Fatalf("transcript:\n%q\nwant\n%q", out.String(), want)
	}
}

func TestLabCheckAndReport(t *testing.T) {
	var out bytes.Buffer
	lab := NewLab(&out)
	exitCode := -1
	lab.exit = func(code int) { exitCode = code }
	lab.Check(true, "yes")
	lab.Report("T")
	if exitCode != -1 || !strings.Contains(out.String(), "T OK") {
		t.Fatalf("passing report: %q exit %d", out.String(), exitCode)
	}
	lab.Check(false, "no")
	lab.Report("T")
	if exitCode != 1 || !strings.Contains(out.String(), "T FAILED: 1 claim(s)") || len(lab.Failures()) != 1 {
		t.Fatalf("failing report: %q exit %d", out.String(), exitCode)
	}
}

func TestProbeSeesAnotherProcessLockAndAStrayCloseDropsIt(t *testing.T) {
	if !LockProbeSupported {
		t.Skip("no fcntl probe on " + runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "locked")
	if err := os.WriteFile(path, make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}
	holder, err := ProbeLock(path, 0, 1000)
	if err != nil || holder.Held {
		t.Fatalf("before: %+v %v", holder, err)
	}
	child, err := StartChild("locker", "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	if result, err := child.Do(1, "lock", path); err != nil || !result.OK() {
		t.Fatalf("lock: %+v %v", result, err)
	}
	states, err := ProbeRanges(path, ShmLockRanges())
	if err != nil {
		t.Fatal(err)
	}
	if !WriteLockedBy(states, "WAL_WRITE_LOCK", child.Pid) || !Unlocked(states, "WAL_CKPT_LOCK", "DMS") || ReadLocked(states, "WAL_WRITE_LOCK") {
		t.Fatalf("states %+v", states)
	}
	lines := FormatRangeStates(states[:2], func(pid int) string { return "locker" })
	if lines[0] != "WAL_WRITE_LOCK     byte 120         write-locked by locker" || lines[1] != "WAL_CKPT_LOCK      byte 121         unlocked" {
		t.Fatalf("lines %q", lines)
	}
	if ProcLocksSupported {
		all, err := ReadProcLocks()
		if err != nil {
			t.Fatal(err)
		}
		inode, err := FileInode(path)
		if err != nil {
			t.Fatal(err)
		}
		mine := FilterProcLocks(all, inode, map[int]bool{child.Pid: true})
		if len(mine) != 1 || mine[0].Type != "WRITE" || mine[0].Start != 120 || mine[0].End != 120 || mine[0].Class != "POSIX" {
			t.Fatalf("/proc/locks entries %+v", mine)
		}
		described := DescribeProcLocks(mine, map[int]string{child.Pid: "locker"}, ShmLockRanges())
		if len(described) != 1 || !strings.HasSuffix(described[0], "WAL_WRITE_LOCK") {
			t.Fatalf("described %q", described)
		}
	}
	if result, err := child.Do(2, "stray-close", path); err != nil || !result.OK() {
		t.Fatalf("stray close: %+v %v", result, err)
	}
	holder, err = ProbeLock(path, 120, 1)
	if err != nil || holder.Held {
		t.Fatalf("after the stray close: %+v %v", holder, err)
	}
	if _, err := ProbeLock(path, -1, 1); err == nil {
		t.Error("negative start accepted")
	}
	if _, err := ProbeLock(path, 0, 0); err == nil {
		t.Error("empty range accepted")
	}
}

func TestParseProcLocks(t *testing.T) {
	text := `1: POSIX  ADVISORY  WRITE 4242 fd:01:786470 1073741825 1073741825
2: -> POSIX  ADVISORY  READ  4243 08:1f:12 1073741826 1073742335
3: FLOCK  ADVISORY  WRITE 99 fd:01:5 0 EOF
4: OFDLCK ADVISORY  READ  -1 00:2e:77 128 128

`
	locks, err := ParseProcLocks(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 4 {
		t.Fatalf("%d locks", len(locks))
	}
	first := locks[0]
	if first.ID != 1 || first.Blocked || first.Class != "POSIX" || first.Type != "WRITE" || first.Pid != 4242 || first.DevMajor != 0xfd || first.DevMinor != 1 || first.Inode != 786470 || first.Start != ReservedByte || first.Length() != 1 || first.RangeText() != "1073741825" {
		t.Errorf("first %+v", first)
	}
	if !locks[1].Blocked || locks[1].DevMinor != 0x1f || locks[1].Length() != SharedSize || locks[1].RangeText() != "1073741826-1073742335" {
		t.Errorf("second %+v", locks[1])
	}
	if !locks[2].ToEOF || locks[2].RangeText() != "0-EOF" {
		t.Errorf("third %+v", locks[2])
	}
	if locks[3].Pid != -1 || locks[3].Class != "OFDLCK" {
		t.Errorf("fourth %+v", locks[3])
	}
	names := map[int]string{4242: "holder", 4243: "other"}
	got := DescribeProcLocks(FilterProcLocks(locks, 786470, map[int]bool{4242: true}), names, DatabaseLockRanges())
	if len(got) != 1 || got[0] != "holder     POSIX WRITE 1073741825            RESERVED" {
		t.Errorf("described %q", got)
	}
	merged := ProcLock{Class: "POSIX", Type: "WRITE", Pid: 4242, Start: 120, End: 121}
	if got := DescribeProcLocks([]ProcLock{merged}, names, ShmLockRanges()); !strings.HasSuffix(got[0], "(WAL_WRITE_LOCK+WAL_CKPT_LOCK)") {
		t.Errorf("merged range %q", got)
	}
}

func TestParseProcLocksRejectsMalformedLines(t *testing.T) {
	bad := []string{
		"",
		"POSIX ADVISORY WRITE 1 fd:01:2 0 0",
		"x: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0",
		"0: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0 extra",
		"1: WEIRD  ADVISORY  WRITE 1 fd:01:2 0 0",
		"1: POSIX  SOMETIMES WRITE 1 fd:01:2 0 0",
		"1: POSIX  ADVISORY  SCRIBBLE 1 fd:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE x fd:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01 0 0",
		"1: POSIX  ADVISORY  WRITE 1 zz:01:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:zz:2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:-2 0 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 -1 0",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 5 4",
		"1: POSIX  ADVISORY  WRITE 1 fd:01:2 5 later",
	}
	for _, line := range bad {
		if _, err := ParseProcLockLine(line); !errors.Is(err, ErrProcLocksFormat) {
			t.Errorf("%q: err %v", line, err)
		}
	}
	if _, err := ParseProcLocks("1: POSIX  ADVISORY  WRITE 1 fd:01:2 0 0\nbroken\n"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("multi-line error %v", err)
	}
}

func TestIntegritySummary(t *testing.T) {
	if IntegrityProblems("ok") != nil || IntegritySummary("ok") != "ok" {
		t.Error("ok")
	}
	text := "*** in database main ***\nPage 3: never used\nPage 4: never used; row 9 missing from index i"
	problems := IntegrityProblems(text)
	if len(problems) != 3 || problems[0] != "Page 3: never used" || problems[2] != "row 9 missing from index i" {
		t.Fatalf("%q", problems)
	}
	if got := IntegritySummary(text); got != "3 problem(s), the first: Page 3: never used" {
		t.Errorf("%q", got)
	}
}

func TestCopierRole(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "src.db")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	actor, err := CopierRole(context.Background(), []string{"-db", db, "-out", out})
	if err != nil {
		t.Fatal(err)
	}
	do := func(action string, args ...string) (string, error) {
		return actor.Actions[action](context.Background(), args)
	}
	if v, err := do("copy-range", "db", "c", "0", "4"); err != nil || v != "bytes 0-4" {
		t.Fatalf("%q %v", v, err)
	}
	if err := os.WriteFile(db, []byte("abcdefghijkl"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := do("copy-range", "db", "c", "4", "end"); err != nil || v != "bytes 4-12" {
		t.Fatalf("%q %v", v, err)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "c")); string(data) != "0123efghijkl" {
		t.Fatalf("torn copy %q", data)
	}
	if v, err := do("copy", "-wal", "w"); err != nil || v != "missing" {
		t.Fatalf("missing source: %q %v", v, err)
	}
	if v, err := do("copy", "db", "whole"); err != nil || v != "12 bytes" {
		t.Fatalf("%q %v", v, err)
	}
	for _, args := range [][]string{{"db", "../x"}, {"db", ""}, {"nope", "x"}} {
		if _, err := do("copy", args...); err == nil {
			t.Errorf("copy %q accepted", args)
		}
	}
	for _, args := range [][]string{{"db", "c", "5", "4"}, {"db", "c", "-1", "end"}, {"db", "c", "x", "end"}, {"db", "c", "0"}} {
		if _, err := do("copy-range", args...); err == nil {
			t.Errorf("copy-range %q accepted", args)
		}
	}
	if _, err := CopierRole(context.Background(), []string{"-db", db}); err == nil {
		t.Error("copier without -out accepted")
	}
}

func TestSQLRolesAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "lab.db")
	if err := CreateDatabase(ctx, db, sqlitex.JournalWAL, "CREATE TABLE lab(id INTEGER PRIMARY KEY, who TEXT)"); err != nil {
		t.Fatal(err)
	}
	options := SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, PagesOut: filepath.Join(dir, "pages.bin")}
	var out bytes.Buffer
	script := NewScript(&out)
	defer script.Kill()
	first, err := script.Spawn("first", RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	second, err := script.Spawn("second", RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	expect := func(child *Child, wantCode int, action string, args ...string) Result {
		t.Helper()
		r, err := script.Do(child, action, args...)
		if err != nil {
			t.Fatal(err)
		}
		if r.Code != wantCode {
			t.Fatalf("%s %s: %s %s", child.Name, action, r.Outcome(), r.Error)
		}
		return r
	}
	expect(first, 0, "begin", "immediate")
	expect(first, 0, "exec", "INSERT INTO lab VALUES (1, 'first')")
	expect(second, sqlitex.CodeBusy, "begin", "immediate")
	expect(second, 0, "begin", "deferred")
	expect(second, 0, "query", "SELECT count(*) FROM lab")
	expect(first, 0, "commit")
	expect(second, sqlitex.CodeBusySnapshot, "exec", "INSERT INTO lab VALUES (2, 'second')")
	expect(second, 0, "rollback")
	if r := expect(second, 0, "query", "SELECT who FROM lab"); r.Value != "first" {
		t.Fatalf("value %q", r.Value)
	}
	if r := expect(second, 0, "query", "SELECT NULL"); r.Value != "NULL" {
		t.Fatalf("null %q", r.Value)
	}
	expect(first, CodeNotSQLite, "begin", "sideways")
	expect(first, CodeNotSQLite, "exec")
	expect(first, CodeNotSQLite, "stray-close", "elsewhere")
	expect(first, 0, "timeout", "10")
	expect(first, CodeNotSQLite, "timeout", "-1")
	if r := expect(first, 0, "checkpoint", "PASSIVE"); !strings.HasPrefix(r.Value, "busy=0 ") {
		t.Fatalf("checkpoint %q", r.Value)
	}
	if r := expect(first, 0, "dbpage-write", "1", "2"); r.Value != "2 pages" {
		t.Fatalf("dbpage-write %q", r.Value)
	}
	expect(first, CodeNotSQLite, "dbpage-write", "2", "1")
	if info, err := os.Stat(options.PagesOut); err != nil || info.Size() != 2*4096 {
		t.Fatalf("pages file %v %v", info, err)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectDatabase(ctx, db, sqlitex.JournalWAL, "SELECT count(*) FROM lab", "SELECT no_such_column FROM lab")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Integrity != "ok" || inspection.Values[0] != "1" || !strings.HasPrefix(inspection.Values[1], "error SQLITE_ERROR") {
		t.Fatalf("%+v", inspection)
	}
}

func TestSQLOptionsRoundTrip(t *testing.T) {
	options := SQLOptions{Path: "/x/y.db", JournalMode: "delete", BusyTimeout: 1500 * time.Millisecond, WALAutocheckpoint: 7, PagesOut: "/x/p"}
	parsed, err := parseSQLOptions(options.Args())
	if err != nil || parsed != options {
		t.Fatalf("%+v %v", parsed, err)
	}
	if _, err := parseSQLOptions([]string{"-journal", "wal"}); err == nil {
		t.Error("missing -db accepted")
	}
	if _, err := parseSQLOptions([]string{"-db", "x", "extra"}); err == nil {
		t.Error("extra argument accepted")
	}
}

func TestRecipeSessionCommitsOnAFreshConnection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("COURSE_WORK_DIR", filepath.Join(dir, "work"))
	db := filepath.Join(dir, "r.db")
	r := recipe.Default().WithSteps(3)
	if err := r.Build(ctx, db, 1); err != nil {
		t.Fatal(err)
	}
	options := SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL, BusyTimeout: time.Second}
	var out bytes.Buffer
	script := NewScript(&out)
	defer script.Kill()
	writer, err := script.Spawn("writer", RoleSQL, options.Args()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"2", "3"} {
		if r, err := script.Do(writer, "recipe-session", k); err != nil || r.Code != 0 {
			t.Fatalf("recipe-session %s: %+v %v", k, r, err)
		}
	}
	if info, err := os.Stat(db + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the held connection should keep the WAL: %v %v", info, err)
	}
	if r, err := script.Do(writer, "user-version"); err != nil || r.Value != "3" {
		t.Fatalf("user-version %+v %v", r, err)
	}
	if r, err := script.Do(writer, "recipe-session", "3"); err != nil || r.Code == 0 {
		t.Fatalf("re-applying step 3 must fail: %+v %v", r, err)
	}
	if r, err := script.Do(writer, "recipe-session", "x"); err != nil || r.Code != CodeNotSQLite {
		t.Fatalf("bad argument: %+v %v", r, err)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	match, err := r.MatchStep(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !match.Found() || match.Step != 3 || !match.Exact {
		t.Fatalf("match %s", match)
	}
}
