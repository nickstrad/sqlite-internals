package govfs

import (
	"reflect"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Function types matching ccgo's translated signatures for sqlite3_vfs
// (sqlite.h.in:1513-1550) and sqlite3_io_methods (sqlite.h.in:854-878).
// Every wrapper must match its type alias at compile time, and
// TestSignaturesMatchTranslatedSource verifies each type against the
// translated modernc.org/sqlite/lib source.
type (
	vfsOpenFunc             = func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32
	vfsDeleteFunc           = func(*libc.TLS, uintptr, uintptr, int32) int32
	vfsAccessFunc           = func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	vfsFullPathnameFunc     = func(*libc.TLS, uintptr, uintptr, int32, uintptr) int32
	vfsRandomnessFunc       = func(*libc.TLS, uintptr, int32, uintptr) int32
	vfsSleepFunc            = func(*libc.TLS, uintptr, int32) int32
	vfsCurrentTimeFunc      = func(*libc.TLS, uintptr, uintptr) int32
	vfsGetLastErrorFunc     = func(*libc.TLS, uintptr, int32, uintptr) int32
	vfsCurrentTimeInt64Func = func(*libc.TLS, uintptr, uintptr) int32

	ioCloseFunc                 = func(*libc.TLS, uintptr) int32
	ioReadFunc                  = func(*libc.TLS, uintptr, uintptr, int32, int64) int32
	ioWriteFunc                 = func(*libc.TLS, uintptr, uintptr, int32, int64) int32
	ioTruncateFunc              = func(*libc.TLS, uintptr, int64) int32
	ioSyncFunc                  = func(*libc.TLS, uintptr, int32) int32
	ioFileSizeFunc              = func(*libc.TLS, uintptr, uintptr) int32
	ioLockFunc                  = func(*libc.TLS, uintptr, int32) int32
	ioUnlockFunc                = func(*libc.TLS, uintptr, int32) int32
	ioCheckReservedLockFunc     = func(*libc.TLS, uintptr, uintptr) int32
	ioFileControlFunc           = func(*libc.TLS, uintptr, int32, uintptr) int32
	ioSectorSizeFunc            = func(*libc.TLS, uintptr) int32
	ioDeviceCharacteristicsFunc = func(*libc.TLS, uintptr) int32
	ioShmMapFunc                = func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32
	ioShmLockFunc               = func(*libc.TLS, uintptr, int32, int32, int32) int32
	ioShmBarrierFunc            = func(*libc.TLS, uintptr)
	ioShmUnmapFunc              = func(*libc.TLS, uintptr, int32) int32
	ioFetchFunc                 = func(*libc.TLS, uintptr, int64, int32, uintptr) int32
	ioUnfetchFunc               = func(*libc.TLS, uintptr, int64, uintptr) int32
)

// Slot describes one intercepted sqlite3_vfs or sqlite3_io_methods method,
// the minimum struct iVersion that defines it, and its expected Go signature.
type Slot struct {
	Method     string
	MinVersion int32
	Signature  reflect.Type
}

type vfsSlot struct {
	Slot
	field   func(*sqlite3.Tsqlite3_vfs) *uintptr
	wrapper uintptr
}

type ioSlot struct {
	Slot
	field   func(*sqlite3.Tsqlite3_io_methods) *uintptr
	wrapper uintptr
}

// newVFSSlot binds a sqlite3_vfs field to a wrapper whose type must equal F at
// compile time, recording F via reflect.TypeFor for the signature test.
func newVFSSlot[F any](method string, minVersion int32, field func(*sqlite3.Tsqlite3_vfs) *uintptr, wrapper F) vfsSlot {
	return vfsSlot{Slot{method, minVersion, reflect.TypeFor[F]()}, field, funcAddress(wrapper)}
}

// newIOSlot binds a sqlite3_io_methods field to a wrapper whose type must equal
// F at compile time, recording F via reflect.TypeFor for the signature test.
func newIOSlot[F any](method string, minVersion int32, field func(*sqlite3.Tsqlite3_io_methods) *uintptr, wrapper F) ioSlot {
	return ioSlot{Slot{method, minVersion, reflect.TypeFor[F]()}, field, funcAddress(wrapper)}
}

// vfsSlots lists the 9 VFS methods wrapped by the shim. Dynamic-library
// (xDl*) and system-call override slots keep the underlying VFS's pointers,
// which ignore their pVfs parameter.
var vfsSlots = []vfsSlot{
	newVFSSlot[vfsOpenFunc]("xOpen", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxOpen }, vfsOpen),
	newVFSSlot[vfsDeleteFunc]("xDelete", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxDelete }, vfsDelete),
	newVFSSlot[vfsAccessFunc]("xAccess", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxAccess }, vfsAccess),
	newVFSSlot[vfsFullPathnameFunc]("xFullPathname", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxFullPathname }, vfsFullPathname),
	newVFSSlot[vfsRandomnessFunc]("xRandomness", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxRandomness }, vfsRandomness),
	newVFSSlot[vfsSleepFunc]("xSleep", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxSleep }, vfsSleep),
	newVFSSlot[vfsCurrentTimeFunc]("xCurrentTime", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxCurrentTime }, vfsCurrentTime),
	newVFSSlot[vfsGetLastErrorFunc]("xGetLastError", 1, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxGetLastError }, vfsGetLastError),
	newVFSSlot[vfsCurrentTimeInt64Func]("xCurrentTimeInt64", 2, func(v *sqlite3.Tsqlite3_vfs) *uintptr { return &v.FxCurrentTimeInt64 }, vfsCurrentTimeInt64),
}

// ioSlots lists all 18 sqlite3_io_methods entries across versions 1 through 3
// (sqlite.h.in:854-878).
var ioSlots = []ioSlot{
	newIOSlot[ioCloseFunc]("xClose", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxClose }, ioClose),
	newIOSlot[ioReadFunc]("xRead", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxRead }, ioRead),
	newIOSlot[ioWriteFunc]("xWrite", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxWrite }, ioWrite),
	newIOSlot[ioTruncateFunc]("xTruncate", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxTruncate }, ioTruncate),
	newIOSlot[ioSyncFunc]("xSync", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxSync }, ioSync),
	newIOSlot[ioFileSizeFunc]("xFileSize", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxFileSize }, ioFileSize),
	newIOSlot[ioLockFunc]("xLock", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxLock }, ioLock),
	newIOSlot[ioUnlockFunc]("xUnlock", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxUnlock }, ioUnlock),
	newIOSlot[ioCheckReservedLockFunc]("xCheckReservedLock", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxCheckReservedLock }, ioCheckReservedLock),
	newIOSlot[ioFileControlFunc]("xFileControl", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxFileControl }, ioFileControl),
	newIOSlot[ioSectorSizeFunc]("xSectorSize", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxSectorSize }, ioSectorSize),
	newIOSlot[ioDeviceCharacteristicsFunc]("xDeviceCharacteristics", 1, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxDeviceCharacteristics }, ioDeviceCharacteristics),
	newIOSlot[ioShmMapFunc]("xShmMap", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmMap }, ioShmMap),
	newIOSlot[ioShmLockFunc]("xShmLock", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmLock }, ioShmLock),
	newIOSlot[ioShmBarrierFunc]("xShmBarrier", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmBarrier }, ioShmBarrier),
	newIOSlot[ioShmUnmapFunc]("xShmUnmap", 2, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxShmUnmap }, ioShmUnmap),
	newIOSlot[ioFetchFunc]("xFetch", 3, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxFetch }, ioFetch),
	newIOSlot[ioUnfetchFunc]("xUnfetch", 3, func(m *sqlite3.Tsqlite3_io_methods) *uintptr { return &m.FxUnfetch }, ioUnfetch),
}

// VFSSlots returns copies of the 9 intercepted sqlite3_vfs slot descriptors.
func VFSSlots() []Slot {
	slots := make([]Slot, len(vfsSlots))
	for i, s := range vfsSlots {
		slots[i] = s.Slot
	}
	return slots
}

// IOSlots returns copies of the 18 intercepted sqlite3_io_methods slot descriptors.
func IOSlots() []Slot {
	slots := make([]Slot, len(ioSlots))
	for i, s := range ioSlots {
		slots[i] = s.Slot
	}
	return slots
}
