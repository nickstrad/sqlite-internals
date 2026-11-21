package govfs

import (
	"path/filepath"
	"testing"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

func cString(t *testing.T, tls *libc.TLS, s string) uintptr {
	t.Helper()
	p, err := libc.CString(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { libc.Xfree(tls, p) })
	return p
}

// TestFileControlVFSNameIncludesShim verifies that SQLITE_FCNTL_VFSNAME on a
// connection opened via govfs returns "<shim>/unix" per sqlite.h.in:1008-1018.
func TestFileControlVFSNameIncludesShim(t *testing.T) {
	v := registerForTest(t, Options{})
	tls := libc.NewTLS()
	defer tls.Close()
	path := cString(t, tls, filepath.Join(t.TempDir(), "t.db"))
	vfsName := cString(t, tls, v.Name())
	schema := cString(t, tls, "main")
	slots := sqlite3.Xsqlite3_malloc64(tls, 16)
	if slots == 0 {
		t.Fatal("out of memory")
	}
	defer sqlite3.Xsqlite3_free(tls, slots)
	dbSlot, nameSlot := slots, slots+8
	*at[uintptr](dbSlot), *at[uintptr](nameSlot) = 0, 0
	if rc := sqlite3.Xsqlite3_open_v2(tls, path, dbSlot, sqlite3.SQLITE_OPEN_READWRITE|sqlite3.SQLITE_OPEN_CREATE, vfsName); rc != sqlite3.SQLITE_OK {
		t.Fatalf("open rc=%d", rc)
	}
	db := *at[uintptr](dbSlot)
	defer sqlite3.Xsqlite3_close(tls, db)
	if rc := sqlite3.Xsqlite3_file_control(tls, db, schema, sqlite3.SQLITE_FCNTL_VFSNAME, nameSlot); rc != sqlite3.SQLITE_OK {
		t.Fatalf("file_control rc=%d", rc)
	}
	got := goString(*at[uintptr](nameSlot))
	sqlite3.Xsqlite3_free(tls, *at[uintptr](nameSlot))
	if want := v.Name() + "/unix"; got != want {
		t.Fatalf("VFSNAME %q, want %q", got, want)
	}
}
