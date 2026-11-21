package govfs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

var testVFSCounter atomic.Int64

func registerForTest(t *testing.T, options Options) *VFS {
	t.Helper()
	name := fmt.Sprintf("govfs-test-%d", testVFSCounter.Add(1))
	v, err := Register(name, options)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func openWith(t *testing.T, path, journal, vfsName string) *sqlitex.DB {
	t.Helper()
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = journal
	o.SingleConnection = true
	o.VFS = vfsName
	db, err := sqlitex.Open(context.Background(), path, o)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func countRows(t *testing.T, path, journal string) (int, string) {
	t.Helper()
	db := openWith(t, path, journal, "")
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatal(err)
	}
	return n, check
}

// TestFunctionPointerConvention verifies funcAddress, funcAt, codeAddress, and typed C-memory reads/writes via at.
func TestFunctionPointerConvention(t *testing.T) {
	if err := checkFunctionPointerConvention(); err != nil {
		t.Fatal(err)
	}
	if got := funcAt[func(int32) int32](funcAddress(selfCheckDouble))(5); got != 10 {
		t.Fatalf("got %d", got)
	}
	tls := libc.NewTLS()
	defer tls.Close()
	address := sqlite3.Xsqlite3_malloc64(tls, 16)
	defer sqlite3.Xsqlite3_free(tls, address)
	*at[uint64](address) = 0x1122334455667788
	*at[int32](address + 8) = -7
	if *at[uint64](address) != 0x1122334455667788 || *at[int32](address + 8) != -7 {
		t.Fatal("at does not read back what it wrote")
	}
	if codeAddress(funcAddress(selfCheckDouble)) == 0 {
		t.Fatal("no code address")
	}
}

// TestRegisterValidation checks empty names, out-of-range io_methods versions, duplicate names, and default version 3.
func TestRegisterValidation(t *testing.T) {
	if _, err := Register("", Options{}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := Register("govfs-bad-version", Options{IOVersion: 4}); err == nil {
		t.Fatal("io version 4 accepted")
	}
	v := registerForTest(t, Options{})
	if _, err := Register(v.Name(), Options{}); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := Register("unix", Options{}); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("unix: %v", err)
	}
	if Lookup(v.Name()) != v {
		t.Fatal("lookup")
	}
	if v.IOVersion() != 3 {
		t.Fatalf("default io version %d", v.IOVersion())
	}
}

// TestForwardingInBothJournalModes confirms the shim forwards and records commits in DELETE and WAL modes and persists rows.
func TestForwardingInBothJournalModes(t *testing.T) {
	for _, journal := range []string{"delete", "wal"} {
		t.Run(journal, func(t *testing.T) {
			v := registerForTest(t, Options{})
			path := filepath.Join(t.TempDir(), "t.db")
			db := openWith(t, path, journal, v.Name())
			if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
				t.Fatal(err)
			}
			v.Reset()
			if _, err := db.Exec("INSERT INTO t VALUES(1, 'one')"); err != nil {
				t.Fatal(err)
			}
			counts := CountCalls(v.Calls())
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if v.Err() != nil {
				t.Fatal(v.Err())
			}
			if counts["xWrite"] == 0 || counts["xSync"] == 0 {
				t.Fatalf("no writes or syncs recorded: %s", counts)
			}
			if journal == "wal" && counts["xShmLock"] == 0 {
				t.Fatalf("WAL commit without xShmLock: %s", counts)
			}
			if journal == "delete" && (counts["xDelete"] != 1 || counts["xShmLock"] != 0) {
				t.Fatalf("rollback commit: %s", counts)
			}
			n, check := countRows(t, path, journal)
			if n != 1 || check != "ok" {
				t.Fatalf("rows=%d integrity=%s", n, check)
			}
		})
	}
}

// TestIOVersionOneCannotUseWAL confirms a version-1 io_methods table omits xShm* so SQLite stays in DELETE mode.
func TestIOVersionOneCannotUseWAL(t *testing.T) {
	v := registerForTest(t, Options{IOVersion: 1})
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", "file:"+path+"?vfs="+v.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=wal").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Fatalf("journal_mode %q, want delete", mode)
	}
	if CountCalls(v.Calls())["xShmMap"] != 0 {
		t.Fatal("xShmMap called through a version-1 method table")
	}
}

// TestFaultInjection verifies injecting SQLITE_IOERR on the first xWrite or xSync aborts the insert and leaves zero rows.
func TestFaultInjection(t *testing.T) {
	for _, journal := range []string{"delete", "wal"} {
		for _, method := range []string{"xWrite", "xSync"} {
			t.Run(journal+"-"+method, func(t *testing.T) {
				v := registerForTest(t, Options{})
				path := filepath.Join(t.TempDir(), "t.db")
				db := openWith(t, path, journal, v.Name())
				if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
					t.Fatal(err)
				}
				if err := v.Arm(Fault{Method: method, Nth: 1, Code: CodeIOErr}); err != nil {
					t.Fatal(err)
				}
				_, err := db.Exec("INSERT INTO t VALUES(1, 'one')")
				report := v.Disarm()
				db.Close()
				if err == nil {
					t.Fatal("insert succeeded despite the fault")
				}
				if code, _ := sqlitex.PrimaryErrorCode(err); code != int(CodeIOErr) {
					t.Fatalf("error %v, primary code %d", err, code)
				}
				if !report.Fired || report.Seen != 1 || report.Call.Method != method || !report.Call.Injected {
					t.Fatalf("report %+v", report)
				}
				n, check := countRows(t, path, journal)
				if n != 0 || check != "ok" {
					t.Fatalf("rows=%d integrity=%s", n, check)
				}
			})
		}
	}
}

// TestFaultKindFilterAndNotReached verifies unreached Nth faults do not fire and Kind filters match only that file role.
func TestFaultKindFilterAndNotReached(t *testing.T) {
	v := registerForTest(t, Options{})
	path := filepath.Join(t.TempDir(), "t.db")
	db := openWith(t, path, "delete", v.Name())
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := v.Arm(Fault{Method: "xWrite", Nth: 1000, Code: CodeIOErr}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	report := v.Disarm()
	if report.Fired || report.Seen == 0 || report.Seen >= 1000 {
		t.Fatalf("report %+v", report)
	}
	if err := v.Arm(Fault{Method: "xWrite", Nth: 1, Code: CodeIOErr, Kind: KindMainDB}); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec("INSERT INTO t VALUES(2)")
	report = v.Disarm()
	if err == nil || report.Call.Kind != KindMainDB {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	if !strings.HasPrefix(report.Call.String(), "xWrite t.db n=4096 ofst=") {
		t.Fatalf("fired call %q", report.Call.String())
	}
}

// TestArmValidation rejects Nth < 1, non-faultable methods, and SQLITE_OK as a fault code.
func TestArmValidation(t *testing.T) {
	v := registerForTest(t, Options{})
	bad := []Fault{
		{Method: "xWrite", Nth: 0, Code: CodeIOErr},
		{Method: "xLock", Nth: 1, Code: CodeIOErr},
		{Method: "xSync", Nth: 1, Code: CodeOK},
	}
	for _, f := range bad {
		if err := v.Arm(f); err == nil {
			t.Fatalf("accepted %+v", f)
		}
	}
	if report := v.Disarm(); report.Armed {
		t.Fatal("armed after rejected faults")
	}
}

// TestRecordingSwitchAndTake checks SetRecording toggling and TakeCalls draining and sequence resetting.
func TestRecordingSwitchAndTake(t *testing.T) {
	v := registerForTest(t, Options{})
	path := filepath.Join(t.TempDir(), "t.db")
	db := openWith(t, path, "wal", v.Name())
	defer db.Close()
	v.SetRecording(false)
	v.Reset()
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if len(v.Calls()) != 0 {
		t.Fatal("recorded while off")
	}
	v.SetRecording(true)
	if _, err := db.Exec("INSERT INTO t VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	calls := v.TakeCalls()
	if len(calls) == 0 || calls[0].Seq != 1 || calls[len(calls)-1].Seq != len(calls) {
		t.Fatalf("calls %v", calls)
	}
	if len(v.Calls()) != 0 {
		t.Fatal("TakeCalls did not clear")
	}
}

// TestStringsAndFlags verifies Call.String, VFSTraceSyncLabel vs SyncFlagsString, KindFromOpenFlags, and Counts.String.
func TestStringsAndFlags(t *testing.T) {
	cases := map[string]Call{
		"xSync t.db-wal flags=NORMAL -> 0":                     {Method: "xSync", File: "t.db-wal", Flags: SyncNormal},
		"xSync t.db flags=FULL|DATAONLY -> 0":                  {Method: "xSync", File: "t.db", Flags: SyncFull | SyncDataOnly},
		"xWrite t.db n=4096 ofst=8192 -> 10 (injected)":        {Method: "xWrite", File: "t.db", Amount: 4096, Offset: 8192, Result: 10, Injected: true},
		"xLock t.db level=RESERVED -> 0":                       {Method: "xLock", File: "t.db", Flags: 2},
		"xShmLock t.db ofst=0 n=1 flags=UNLOCK|EXCLUSIVE -> 0": {Method: "xShmLock", File: "t.db", Amount: 1, Flags: ShmUnlock | ShmExclusive},
		"xOpen t.db-wal kind=wal flags=0x80006 -> 0":           {Method: "xOpen", File: "t.db-wal", Kind: KindWAL, Flags: 0x80006},
		"xFileSize t.db -> 0 size=8192":                        {Method: "xFileSize", File: "t.db", Out: 8192},
		"xAccess t.db-journal flags=0 -> 0 out=0":              {Method: "xAccess", File: "t.db-journal"},
	}
	for want, c := range cases {
		if got := c.String(); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if VFSTraceSyncLabel(SyncNormal) != "FULL" || SyncFlagsString(SyncNormal) != "NORMAL" {
		t.Fatal("vfstrace label of SQLITE_SYNC_NORMAL should be FULL, the real flag NORMAL")
	}
	if VFSTraceSyncLabel(SyncNormal|SyncDataOnly) != "FULL|DATAONLY" {
		t.Fatal(VFSTraceSyncLabel(SyncNormal | SyncDataOnly))
	}
	kinds := map[int32]FileKind{OpenMainDB | OpenCreate: KindMainDB, OpenWAL: KindWAL, OpenMainJournal: KindMainJournal, OpenTempJournal: KindTempJournal, OpenSubjournal: KindSubjournal, 0: KindOther}
	for flags, want := range kinds {
		if got := KindFromOpenFlags(flags); got != want {
			t.Errorf("flags 0x%x: %s want %s", flags, got, want)
		}
	}
	if s := (Counts{"xWrite": 2, "xSync": 1}).String(); s != "xSync=1 xWrite=2" {
		t.Fatal(s)
	}
}
