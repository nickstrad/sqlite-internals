// session: record a changeset with the session extension while running a SQL script against
// db A, then apply that changeset to db B (a copy of A's starting state).
// cgo against a session-enabled build of the pinned tree (configure --enable-all):
//   CGO_CFLAGS=-I$BLD CGO_LDFLAGS="-L$BLD" go build ./session
// Usage: session A.db SCRIPT.sql B.db CHANGESET.out
package main

/*
#cgo LDFLAGS: -l:libsqlite3.a -lm -lz
#define SQLITE_ENABLE_SESSION 1
#define SQLITE_ENABLE_PREUPDATE_HOOK 1
#include <sqlite3.h>
#include <stdlib.h>
static int abortOnConflict(void *p, int eConflict, sqlite3_changeset_iter *it){
  return SQLITE_CHANGESET_ABORT;
}
static int apply(sqlite3 *db, int n, void *p){
  return sqlite3changeset_apply(db, n, p, 0, abortOnConflict, 0);
}
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"
)

func open(path string) *C.sqlite3 {
	var db *C.sqlite3
	cs := C.CString(path)
	defer C.free(unsafe.Pointer(cs))
	if rc := C.sqlite3_open(cs, &db); rc != C.SQLITE_OK {
		panic(fmt.Sprintf("open %s rc=%d", path, rc))
	}
	return db
}

func exec(db *C.sqlite3, sql string) {
	cs := C.CString(sql)
	defer C.free(unsafe.Pointer(cs))
	if rc := C.sqlite3_exec(db, cs, nil, nil, nil); rc != C.SQLITE_OK {
		panic(fmt.Sprintf("exec rc=%d: %s", rc, C.GoString(C.sqlite3_errmsg(db))))
	}
}

func main() {
	script, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	a := open(os.Args[1])
	var s *C.sqlite3_session
	main := C.CString("main")
	defer C.free(unsafe.Pointer(main))
	if rc := C.sqlite3session_create(a, main, &s); rc != C.SQLITE_OK {
		panic(rc)
	}
	C.sqlite3session_attach(s, nil) // all tables
	exec(a, string(script))
	var n C.int
	var p unsafe.Pointer
	if rc := C.sqlite3session_changeset(s, &n, &p); rc != C.SQLITE_OK {
		panic(rc)
	}
	cs := C.GoBytes(p, n)
	if err := os.WriteFile(os.Args[4], cs, 0o644); err != nil {
		panic(err)
	}
	C.sqlite3session_delete(s)
	C.sqlite3_close(a)

	b := open(os.Args[3])
	rc := C.apply(b, n, p)
	C.sqlite3_free(p)
	C.sqlite3_close(b)
	fmt.Printf("changeset %d bytes; apply rc=%d\n", len(cs), rc)
}
