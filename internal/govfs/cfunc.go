package govfs

import "unsafe"

func funcAddress(f any) uintptr {
	return (*[2]uintptr)(unsafe.Pointer(&f))[1]
}

func funcAt[F any](address uintptr) F {
	return *(*F)(unsafe.Pointer(&address))
}

func at[T any](address uintptr) *T {
	return *(**T)(unsafe.Pointer(&address))
}

func codeAddress(funcAddress uintptr) uintptr {
	return *at[uintptr](funcAddress)
}

const pointerSize = unsafe.Sizeof(uintptr(0))
