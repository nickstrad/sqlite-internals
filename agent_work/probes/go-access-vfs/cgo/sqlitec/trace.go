package sqlitec

/*
#include "sqlite3.h"
#include "glue.h"
#include <stdlib.h>
*/
import "C"

import (
	"strings"
	"sync"
	"unsafe"
)

var (
	traceMu   sync.Mutex
	tracePart strings.Builder
	// TraceLine receives each complete vfstrace line (call + " -> rc").
	TraceLine = func(string) {}
	// WalHook receives sqlite3_wal_hook callbacks (schema, frames in WAL).
	WalHook = func(string, int) {}
)

//export goVfsTrace
func goVfsTrace(z *C.char) {
	traceMu.Lock()
	defer traceMu.Unlock()
	tracePart.WriteString(C.GoString(z))
	s := tracePart.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		TraceLine(s[:i])
		s = s[i+1:]
	}
	tracePart.Reset()
	tracePart.WriteString(s)
}

//export goWalHook
func goWalHook(z *C.char, n C.int) { WalHook(C.GoString(z), int(n)) }

// RegisterVfsTrace registers the tree's vfstrace shim over the default ("unix") VFS.
func RegisterVfsTrace(name string, makeDefault bool) error {
	cn := C.CString(name) // kept alive: vfstrace copies the name, but keep it simple
	defer C.free(unsafe.Pointer(cn))
	d := C.int(0)
	if makeDefault {
		d = 1
	}
	if rc := C.sqlitec_vfstrace_register(cn, d); rc != C.SQLITE_OK {
		return (&DB{}).errNoDB(rc)
	}
	return nil
}

func (db *DB) InstallWalHook() { C.sqlitec_wal_hook_install(db.p) }

func (db *DB) errNoDB(rc C.int) error { return &rcError{int(rc), C.GoString(C.sqlite3_errstr(rc))} }

type rcError struct {
	rc  int
	msg string
}

func (e *rcError) Error() string { return e.msg }
