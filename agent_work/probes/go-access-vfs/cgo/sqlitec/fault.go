package sqlitec

/*
#include "sqlite3.h"
#include "glue.h"
#include <stdlib.h>
*/
import "C"

import "unsafe"

// RegisterFaultVfs registers faultvfs.c over the default VFS.
func RegisterFaultVfs(name string, makeDefault bool) error {
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	d := C.int(0)
	if makeDefault {
		d = 1
	}
	if rc := C.sqlitec_faultvfs_register(cn, d); rc != C.SQLITE_OK {
		return (&DB{}).errNoDB(rc)
	}
	return nil
}

// SetFaults arms the shim (0 = off) and resets the counters.
func SetFaults(failWriteAt, failSyncAt, exitAtSync int) {
	C.sqlitec_fault_set(C.int(failWriteAt), C.int(failSyncAt), C.int(exitAtSync))
}

// FaultCounts returns xWrite and xSync calls seen since SetFaults.
func FaultCounts() (nWrite, nSync int) {
	var w, s C.int
	C.sqlitec_fault_counts(&w, &s)
	return int(w), int(s)
}
