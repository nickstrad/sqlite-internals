// Package mattnshim registers the tree's ext/misc/vfstrace.c into whatever SQLite the binary
// links: mattn/go-sqlite3's bundled amalgamation, or (with -tags libsqlite3) the pinned
// /usr/local/lib/libsqlite3. It needs no sqlite3* handle: sqlite3_vfs_register is global, and
// connections pick the VFS with "file:...?vfs=trace". Header: the pinned /usr/local/include.
package mattnshim

/*
#cgo CFLAGS: -I/usr/local/include -I${SRCDIR}/../../cgo/sqlitec
int mattnshim_register(void);
*/
import "C"

import "fmt"

// Line receives raw vfstrace output fragments.
var Line = func(string) {}

//export goShimTrace
func goShimTrace(z *C.char) { Line(C.GoString(z)) }

func Register() error {
	if rc := C.mattnshim_register(); rc != 0 {
		return fmt.Errorf("vfstrace_register rc=%d", rc)
	}
	return nil
}
