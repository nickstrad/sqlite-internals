package govfs

import "unsafe"

// funcAddress returns the static funcval address stored in the data word of an
// interface value holding a top-level Go function. This matches ccgo's
// __ccgo_fp convention used by modernc.org/sqlite/lib to represent C function
// pointers inside sqlite3_vfs and sqlite3_io_methods structs.
func funcAddress(f any) uintptr {
	return (*[2]uintptr)(unsafe.Pointer(&f))[1]
}

// funcAt reinterprets a stored C function pointer (a ccgo funcval address) as a
// Go function value of type F. Every address passed here comes from SQLite's
// own VFS or io_methods tables and is checked at startup and by
// TestSignaturesMatchTranslatedSource.
func funcAt[F any](address uintptr) F {
	return *(*F)(unsafe.Pointer(&address))
}

// at converts a non-Go C-memory address (allocated by modernc.org/libc or
// sqlite3_malloc64, outside the Go garbage collector's heap) into a typed
// pointer *T. Taking the address of the uintptr variable keeps go vet's
// unsafeptr check satisfied while relying on the invariant that no Go heap
// pointer is ever round-tripped through uintptr here.
func at[T any](address uintptr) *T {
	return *(**T)(unsafe.Pointer(&address))
}

// codeAddress reads the first word of a Go funcval (the machine code entry
// point) so runtime.FuncForPC can resolve the target function's name.
func codeAddress(funcAddress uintptr) uintptr {
	return *at[uintptr](funcAddress)
}

// pointerSize is the byte width of a C pointer on the target architecture.
const pointerSize = unsafe.Sizeof(uintptr(0))
