// serialize: sqlite3_serialize the "main" schema of a database into a byte slice, write it to
// OUT, then sqlite3_deserialize it into a fresh in-memory connection and serialize again.
// cgo against the installed pinned library (/usr/local/lib/libsqlite3, /usr/local/include).
// Usage: serialize SRC.db OUT.db
package main

/*
#cgo CFLAGS: -I/usr/local/include
#cgo LDFLAGS: -L/usr/local/lib -lsqlite3 -Wl,-rpath,/usr/local/lib
#include <sqlite3.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"bytes"
	"crypto/sha256"
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

func serialize(db *C.sqlite3) []byte {
	var sz C.sqlite3_int64
	main := C.CString("main")
	defer C.free(unsafe.Pointer(main))
	p := C.sqlite3_serialize(db, main, &sz, 0)
	if p == nil {
		panic("serialize returned NULL")
	}
	defer C.sqlite3_free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(sz))
}

func main() {
	db := open(os.Args[1])
	img := serialize(db)
	C.sqlite3_close(db)
	must(os.WriteFile(os.Args[2], img, 0o644))
	fmt.Printf("serialized %d bytes sha=%x\n", len(img), sha256.Sum256(img))

	// deserialize into :memory: (SQLite takes ownership of an sqlite3_malloc'd copy)
	mem := open(":memory:")
	buf := C.sqlite3_malloc64(C.sqlite3_uint64(len(img)))
	C.memcpy(buf, unsafe.Pointer(&img[0]), C.size_t(len(img)))
	main := C.CString("main")
	rc := C.sqlite3_deserialize(mem, main, (*C.uchar)(buf), C.sqlite3_int64(len(img)), C.sqlite3_int64(len(img)),
		C.SQLITE_DESERIALIZE_FREEONCLOSE|C.SQLITE_DESERIALIZE_RESIZEABLE)
	C.free(unsafe.Pointer(main))
	if rc != C.SQLITE_OK {
		panic(fmt.Sprintf("deserialize rc=%d", rc))
	}
	again := serialize(mem)
	fmt.Printf("deserialize->serialize round trip identical: %v\n", bytes.Equal(img, again))
	C.sqlite3_close(mem)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
