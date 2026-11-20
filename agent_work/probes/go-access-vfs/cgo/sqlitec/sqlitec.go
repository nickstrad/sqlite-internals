// Package sqlitec is a minimal first-party cgo binding to the SQLite amalgamation generated from
// the pinned tree (compiled in ../amalg; run ../../gen-amalgamation.sh first). It exists to show how thin such a
// wrapper can be and that it exposes every C API the course needs. Not a database/sql driver.
package sqlitec

/*
#cgo CFLAGS: -O2 -I${SRCDIR}/../amalg
#include <stdlib.h>
#include "sqlite3.h"
#include "glue.h"
*/
import "C"

import (
	"errors"

	_ "sqliteinternals/probes/goaccessvfs/cgo/amalg"

	"fmt"
	"unsafe"
)

type DB struct{ p *C.sqlite3 }

func SourceID() string   { return C.GoString(C.sqlite3_sourceid()) }
func LibVersion() string { return C.GoString(C.sqlite3_libversion()) }

func (db *DB) err(rc C.int) error {
	if rc == C.SQLITE_OK || rc == C.SQLITE_ROW || rc == C.SQLITE_DONE {
		return nil
	}
	return fmt.Errorf("sqlite rc=%d (%s): %s", int(rc), C.GoString(C.sqlite3_errstr(rc)), C.GoString(C.sqlite3_errmsg(db.p)))
}

// Open calls sqlite3_open_v2. vfs "" means the default VFS.
func Open(path string, vfs string) (*DB, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var cvfs *C.char
	if vfs != "" {
		cvfs = C.CString(vfs)
		defer C.free(unsafe.Pointer(cvfs))
	}
	db := &DB{}
	rc := C.sqlite3_open_v2(cpath, &db.p, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_URI, cvfs)
	if rc != C.SQLITE_OK {
		err := db.err(rc)
		C.sqlite3_close_v2(db.p)
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error { return db.err(C.sqlite3_close_v2(db.p)) }

// Exec runs one or more statements via sqlite3_exec.
func (db *DB) Exec(sql string) error {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	return db.err(C.sqlite3_exec(db.p, csql, nil, nil, nil))
}

// Query runs one statement with sqlite3_prepare_v2/step and returns rows of Go values.
func (db *DB) Query(sql string) ([][]any, error) {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	var st *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(db.p, csql, -1, &st, nil); rc != C.SQLITE_OK {
		return nil, db.err(rc)
	}
	defer C.sqlite3_finalize(st)
	var out [][]any
	for {
		rc := C.sqlite3_step(st)
		if rc == C.SQLITE_DONE {
			return out, nil
		}
		if rc != C.SQLITE_ROW {
			return out, db.err(rc)
		}
		n := int(C.sqlite3_column_count(st))
		row := make([]any, n)
		for i := 0; i < n; i++ {
			ci := C.int(i)
			switch C.sqlite3_column_type(st, ci) {
			case C.SQLITE_INTEGER:
				row[i] = int64(C.sqlite3_column_int64(st, ci))
			case C.SQLITE_FLOAT:
				row[i] = float64(C.sqlite3_column_double(st, ci))
			case C.SQLITE_NULL:
				row[i] = nil
			case C.SQLITE_BLOB:
				row[i] = C.GoBytes(C.sqlite3_column_blob(st, ci), C.sqlite3_column_bytes(st, ci))
			default:
				row[i] = C.GoStringN((*C.char)(unsafe.Pointer(C.sqlite3_column_text(st, ci))), C.sqlite3_column_bytes(st, ci))
			}
		}
		out = append(out, row)
	}
}

// CheckpointV2 calls sqlite3_wal_checkpoint_v2 (mode: 0 passive,1 full,2 restart,3 truncate).
func (db *DB) CheckpointV2(schema string, mode int) (nLog, nCkpt int, err error) {
	cs := C.CString(schema)
	defer C.free(unsafe.Pointer(cs))
	var l, c C.int
	rc := C.sqlite3_wal_checkpoint_v2(db.p, cs, C.int(mode), &l, &c)
	return int(l), int(c), db.err(rc)
}

// FileControlInt calls sqlite3_file_control with an int argument and returns the value written back.
func (db *DB) FileControlInt(schema string, op, arg int) (int, error) {
	cs := C.CString(schema)
	defer C.free(unsafe.Pointer(cs))
	v := C.int(arg)
	rc := C.sqlite3_file_control(db.p, cs, C.int(op), unsafe.Pointer(&v))
	return int(v), db.err(rc)
}

const (
	FcntlPersistWAL  = int(C.SQLITE_FCNTL_PERSIST_WAL)
	FcntlDataVersion = int(C.SQLITE_FCNTL_DATA_VERSION)
)

// Snapshot wraps sqlite3_snapshot (needs SQLITE_ENABLE_SNAPSHOT, set above).
type Snapshot struct{ p *C.sqlite3_snapshot }

func (db *DB) SnapshotGet(schema string) (*Snapshot, error) {
	cs := C.CString(schema)
	defer C.free(unsafe.Pointer(cs))
	s := &Snapshot{}
	return s, db.err(C.sqlite3_snapshot_get(db.p, cs, &s.p))
}

func (db *DB) SnapshotOpen(schema string, s *Snapshot) error {
	cs := C.CString(schema)
	defer C.free(unsafe.Pointer(cs))
	return db.err(C.sqlite3_snapshot_open(db.p, cs, s.p))
}

func (s *Snapshot) Free() { C.sqlite3_snapshot_free(s.p) }

// Serialize calls sqlite3_serialize and copies the image into Go memory.
func (db *DB) Serialize(schema string) []byte {
	cs := C.CString(schema)
	defer C.free(unsafe.Pointer(cs))
	var n C.sqlite3_int64
	p := C.sqlite3_serialize(db.p, cs, &n, 0)
	if p == nil {
		return nil
	}
	defer C.sqlite3_free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(n))
}

// DBStatus wraps sqlite3_db_status.
func (db *DB) DBStatus(op int, reset bool) (cur, hi int, err error) {
	var c, h, r C.int
	if reset {
		r = 1
	}
	rc := C.sqlite3_db_status(db.p, C.int(op), &c, &h, r)
	return int(c), int(h), db.err(rc)
}

const DBStatusCacheWrite = int(C.SQLITE_DBSTATUS_CACHE_WRITE)

var ErrNotFound = errors.New("not found")
