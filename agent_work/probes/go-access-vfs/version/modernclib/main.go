// Calls the transpiled C API of modernc.org/sqlite/lib directly (no database/sql), to show that
// APIs the driver does not wrap (here sqlite3_wal_checkpoint_v2 and sqlite3_snapshot_get) are
// reachable, and what that code looks like (uintptr + libc.TLS, C memory by hand).
// Usage: go run ./version/modernclib
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"unsafe"

	"modernc.org/libc"
	lib "modernc.org/sqlite/lib"
)

func main() {
	tls := libc.NewTLS()
	defer tls.Close()
	fmt.Println("sqlite3_sourceid:", libc.GoString(lib.Xsqlite3_sourceid(tls)))
	dir, _ := os.MkdirTemp("", "modernclib")
	defer os.RemoveAll(dir)
	cpath, _ := libc.CString(filepath.Join(dir, "t.db"))
	defer libc.Xfree(tls, cpath)
	pdb := libc.Xmalloc(tls, 8)
	defer libc.Xfree(tls, pdb)
	if rc := lib.Xsqlite3_open_v2(tls, cpath, pdb, lib.SQLITE_OPEN_READWRITE|lib.SQLITE_OPEN_CREATE, 0); rc != 0 {
		log.Fatal("open rc=", rc)
	}
	db := *(*uintptr)(unsafe.Pointer(pdb))
	exec := func(sql string) {
		c, _ := libc.CString(sql)
		defer libc.Xfree(tls, c)
		if rc := lib.Xsqlite3_exec(tls, db, c, 0, 0, 0); rc != 0 {
			log.Fatalf("%s: rc=%d %s", sql, rc, libc.GoString(lib.Xsqlite3_errmsg(tls, db)))
		}
	}
	exec("pragma journal_mode=wal; pragma wal_autocheckpoint=0; create table t(x); insert into t values(1);")
	out := libc.Xmalloc(tls, 8)
	defer libc.Xfree(tls, out)
	main_, _ := libc.CString("main")
	defer libc.Xfree(tls, main_)
	rc := lib.Xsqlite3_wal_checkpoint_v2(tls, db, main_, lib.SQLITE_CHECKPOINT_PASSIVE, out, out+4)
	fmt.Printf("wal_checkpoint_v2 rc=%d nLog=%d nCkpt=%d\n", rc, *(*int32)(unsafe.Pointer(out)), *(*int32)(unsafe.Pointer(out + 4)))
	exec("begin; select count(*) from t;")
	psnap := libc.Xmalloc(tls, 8)
	defer libc.Xfree(tls, psnap)
	rc = lib.Xsqlite3_snapshot_get(tls, db, main_, psnap)
	fmt.Println("sqlite3_snapshot_get rc =", rc)
	if rc == 0 {
		lib.Xsqlite3_snapshot_free(tls, *(*uintptr)(unsafe.Pointer(psnap)))
	}
	exec("commit")
	lib.Xsqlite3_close_v2(tls, db)
}
