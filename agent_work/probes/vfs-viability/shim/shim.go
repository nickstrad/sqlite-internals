package shim

import (
	"fmt"
	"sync"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

type header struct {
	base sqlite3.Tsqlite3_file
	id   uintptr
}

const headerSize = unsafe.Sizeof(header{})

var (
	mu          sync.Mutex
	events      []string
	names       = map[uintptr]string{}
	nextID      uintptr
	realVFS     uintptr
	realMethods = map[uintptr]uintptr{}
	shimVFS     sqlite3.Tsqlite3_vfs
	methods     sqlite3.Tsqlite3_io_methods
	registered  bool
)

func funcPointer(f any) uintptr {
	type iface [2]uintptr
	return (*iface)(unsafe.Pointer(&f))[1]
}

func record(format string, args ...any) {
	mu.Lock()
	events = append(events, fmt.Sprintf(format, args...))
	mu.Unlock()
}

type fault struct {
	method string
	nth    int
	seen   int
	rc     int32
	fired  bool
}

var activeFault *fault

func FailNth(method string, nth int, rc int32) {
	mu.Lock()
	activeFault = &fault{method: method, nth: nth, rc: rc}
	mu.Unlock()
}

func ClearFault() bool {
	mu.Lock()
	defer mu.Unlock()
	fired := activeFault != nil && activeFault.fired
	activeFault = nil
	return fired
}

func injected(method string) (int32, bool) {
	mu.Lock()
	defer mu.Unlock()
	if activeFault == nil || activeFault.method != method || activeFault.fired {
		return 0, false
	}
	activeFault.seen++
	if activeFault.seen == activeFault.nth {
		activeFault.fired = true
		return activeFault.rc, true
	}
	return 0, false
}

func Events() []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), events...)
}

func Reset() {
	mu.Lock()
	events = nil
	mu.Unlock()
}

func inner(pFile uintptr) uintptr {
	return pFile + headerSize
}

func innerMethods(pFile uintptr) *sqlite3.Tsqlite3_io_methods {
	return (*sqlite3.Tsqlite3_io_methods)(unsafe.Pointer((*sqlite3.Tsqlite3_file)(unsafe.Pointer(inner(pFile))).FpMethods))
}

func fileName(pFile uintptr) string {
	mu.Lock()
	defer mu.Unlock()
	return names[(*header)(unsafe.Pointer(pFile)).id]
}

func Register(name string, ioVersion int32, makeDefault bool) error {
	mu.Lock()
	defer mu.Unlock()
	if registered {
		return fmt.Errorf("already registered")
	}
	tls := libc.NewTLS()
	defer tls.Close()
	realVFS = sqlite3.Xsqlite3_vfs_find(tls, 0)
	if realVFS == 0 {
		return fmt.Errorf("no default VFS")
	}
	shimVFS = *(*sqlite3.Tsqlite3_vfs)(unsafe.Pointer(realVFS))
	cname, err := libc.CString(name)
	if err != nil {
		return err
	}
	shimVFS.FzName = cname
	shimVFS.FpNext = 0
	shimVFS.FszOsFile = int32(headerSize) + shimVFS.FszOsFile
	shimVFS.FxOpen = funcPointer(xOpen)
	shimVFS.FxDelete = funcPointer(xDelete)
	shimVFS.FxAccess = funcPointer(xAccess)
	methods = sqlite3.Tsqlite3_io_methods{
		FiVersion:               ioVersion,
		FxClose:                 funcPointer(xClose),
		FxRead:                  funcPointer(xRead),
		FxWrite:                 funcPointer(xWrite),
		FxTruncate:              funcPointer(xTruncate),
		FxSync:                  funcPointer(xSync),
		FxFileSize:              funcPointer(xFileSize),
		FxLock:                  funcPointer(xLock),
		FxUnlock:                funcPointer(xUnlock),
		FxCheckReservedLock:     funcPointer(xCheckReservedLock),
		FxFileControl:           funcPointer(xFileControl),
		FxSectorSize:            funcPointer(xSectorSize),
		FxDeviceCharacteristics: funcPointer(xDeviceCharacteristics),
	}
	if ioVersion >= 2 {
		methods.FxShmMap = funcPointer(xShmMap)
		methods.FxShmLock = funcPointer(xShmLock)
		methods.FxShmBarrier = funcPointer(xShmBarrier)
		methods.FxShmUnmap = funcPointer(xShmUnmap)
	}
	if ioVersion >= 3 {
		methods.FxFetch = funcPointer(xFetch)
		methods.FxUnfetch = funcPointer(xUnfetch)
	}
	if rc := sqlite3.Xsqlite3_vfs_register(tls, uintptr(unsafe.Pointer(&shimVFS)), libc.Bool32(makeDefault)); rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sqlite3_vfs_register: %d", rc)
	}
	registered = true
	return nil
}

func realVFSStruct() *sqlite3.Tsqlite3_vfs {
	return (*sqlite3.Tsqlite3_vfs)(unsafe.Pointer(realVFS))
}

func xOpen(tls *libc.TLS, pVfs uintptr, zName uintptr, pFile uintptr, flags int32, pOutFlags uintptr) int32 {
	name := "<temp>"
	if zName != 0 {
		name = libc.GoString(zName)
	}
	(*header)(unsafe.Pointer(pFile)).base.FpMethods = 0
	open := *(*func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&realVFSStruct().FxOpen))
	rc := open(tls, realVFS, zName, inner(pFile), flags, pOutFlags)
	record("xOpen %s flags=0x%x rc=%d", name, flags, rc)
	if (*sqlite3.Tsqlite3_file)(unsafe.Pointer(inner(pFile))).FpMethods == 0 {
		return rc
	}
	mu.Lock()
	nextID++
	names[nextID] = name
	(*header)(unsafe.Pointer(pFile)).id = nextID
	mu.Unlock()
	(*header)(unsafe.Pointer(pFile)).base.FpMethods = uintptr(unsafe.Pointer(&methods))
	return rc
}

func xDelete(tls *libc.TLS, pVfs uintptr, zName uintptr, syncDir int32) int32 {
	del := *(*func(*libc.TLS, uintptr, uintptr, int32) int32)(unsafe.Pointer(&realVFSStruct().FxDelete))
	rc := del(tls, realVFS, zName, syncDir)
	record("xDelete %s syncDir=%d rc=%d", libc.GoString(zName), syncDir, rc)
	return rc
}

func xAccess(tls *libc.TLS, pVfs uintptr, zName uintptr, flags int32, pResOut uintptr) int32 {
	access := *(*func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&realVFSStruct().FxAccess))
	return access(tls, realVFS, zName, flags, pResOut)
}

func xClose(tls *libc.TLS, pFile uintptr) int32 {
	name := fileName(pFile)
	call := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxClose))
	rc := call(tls, inner(pFile))
	record("xClose %s rc=%d", name, rc)
	mu.Lock()
	delete(names, (*header)(unsafe.Pointer(pFile)).id)
	mu.Unlock()
	return rc
}

func xRead(tls *libc.TLS, pFile uintptr, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	call := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&innerMethods(pFile).FxRead))
	rc := call(tls, inner(pFile), zBuf, iAmt, iOfst)
	record("xRead %s n=%d ofst=%d rc=%d", fileName(pFile), iAmt, iOfst, rc)
	return rc
}

func xWrite(tls *libc.TLS, pFile uintptr, zBuf uintptr, iAmt int32, iOfst int64) int32 {
	if rc, fail := injected("xWrite"); fail {
		record("xWrite %s n=%d ofst=%d rc=%d injected", fileName(pFile), iAmt, iOfst, rc)
		return rc
	}
	call := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&innerMethods(pFile).FxWrite))
	rc := call(tls, inner(pFile), zBuf, iAmt, iOfst)
	record("xWrite %s n=%d ofst=%d rc=%d", fileName(pFile), iAmt, iOfst, rc)
	return rc
}

func xTruncate(tls *libc.TLS, pFile uintptr, size int64) int32 {
	call := *(*func(*libc.TLS, uintptr, int64) int32)(unsafe.Pointer(&innerMethods(pFile).FxTruncate))
	rc := call(tls, inner(pFile), size)
	record("xTruncate %s size=%d rc=%d", fileName(pFile), size, rc)
	return rc
}

func xSync(tls *libc.TLS, pFile uintptr, flags int32) int32 {
	if rc, fail := injected("xSync"); fail {
		record("xSync %s flags=0x%x rc=%d injected", fileName(pFile), flags, rc)
		return rc
	}
	call := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&innerMethods(pFile).FxSync))
	rc := call(tls, inner(pFile), flags)
	record("xSync %s flags=0x%x rc=%d", fileName(pFile), flags, rc)
	return rc
}

func xFileSize(tls *libc.TLS, pFile uintptr, pSize uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxFileSize))
	return call(tls, inner(pFile), pSize)
}

func xLock(tls *libc.TLS, pFile uintptr, level int32) int32 {
	call := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&innerMethods(pFile).FxLock))
	rc := call(tls, inner(pFile), level)
	record("xLock %s level=%d rc=%d", fileName(pFile), level, rc)
	return rc
}

func xUnlock(tls *libc.TLS, pFile uintptr, level int32) int32 {
	call := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&innerMethods(pFile).FxUnlock))
	rc := call(tls, inner(pFile), level)
	record("xUnlock %s level=%d rc=%d", fileName(pFile), level, rc)
	return rc
}

func xCheckReservedLock(tls *libc.TLS, pFile uintptr, pResOut uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxCheckReservedLock))
	return call(tls, inner(pFile), pResOut)
}

func xFileControl(tls *libc.TLS, pFile uintptr, op int32, pArg uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr, int32, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxFileControl))
	rc := call(tls, inner(pFile), op, pArg)
	record("xFileControl %s op=%d rc=%d", fileName(pFile), op, rc)
	return rc
}

func xSectorSize(tls *libc.TLS, pFile uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxSectorSize))
	return call(tls, inner(pFile))
}

func xDeviceCharacteristics(tls *libc.TLS, pFile uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxDeviceCharacteristics))
	return call(tls, inner(pFile))
}

func xShmMap(tls *libc.TLS, pFile uintptr, region int32, size int32, extend int32, pp uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxShmMap))
	rc := call(tls, inner(pFile), region, size, extend, pp)
	record("xShmMap %s region=%d size=%d extend=%d rc=%d", fileName(pFile), region, size, extend, rc)
	return rc
}

func xShmLock(tls *libc.TLS, pFile uintptr, offset int32, n int32, flags int32) int32 {
	call := *(*func(*libc.TLS, uintptr, int32, int32, int32) int32)(unsafe.Pointer(&innerMethods(pFile).FxShmLock))
	rc := call(tls, inner(pFile), offset, n, flags)
	record("xShmLock %s ofst=%d n=%d flags=%d rc=%d", fileName(pFile), offset, n, flags, rc)
	return rc
}

func xShmBarrier(tls *libc.TLS, pFile uintptr) {
	call := *(*func(*libc.TLS, uintptr))(unsafe.Pointer(&innerMethods(pFile).FxShmBarrier))
	call(tls, inner(pFile))
}

func xShmUnmap(tls *libc.TLS, pFile uintptr, deleteFlag int32) int32 {
	call := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&innerMethods(pFile).FxShmUnmap))
	rc := call(tls, inner(pFile), deleteFlag)
	record("xShmUnmap %s delete=%d rc=%d", fileName(pFile), deleteFlag, rc)
	return rc
}

func xFetch(tls *libc.TLS, pFile uintptr, iOfst int64, iAmt int32, pp uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr, int64, int32, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxFetch))
	return call(tls, inner(pFile), iOfst, iAmt, pp)
}

func xUnfetch(tls *libc.TLS, pFile uintptr, iOfst int64, p uintptr) int32 {
	call := *(*func(*libc.TLS, uintptr, int64, uintptr) int32)(unsafe.Pointer(&innerMethods(pFile).FxUnfetch))
	return call(tls, inner(pFile), iOfst, p)
}
