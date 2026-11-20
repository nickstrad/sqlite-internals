package govfs

import (
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

const codeCantOpen int32 = 14

func goString(z uintptr) string {
	if z == 0 {
		return ""
	}
	return libc.GoString(z)
}

func vfsOpen(tls *libc.TLS, pVfs uintptr, zName uintptr, pFile uintptr, flags int32, pOutFlags uintptr) int32 {
	v := vfsFor(pVfs)
	path := goString(zName)
	header := at[sqlite3.Tsqlite3_file](pFile)
	header.FpMethods = 0
	rc := funcAt[vfsOpenFunc](realVFS().FxOpen)(tls, registry.realVFS, zName, inner(pFile), flags, pOutFlags)
	kind := KindFromOpenFlags(flags)
	call := Call{Method: "xOpen", File: displayName(path), Kind: kind, Flags: int64(flags), Result: rc}
	innerMethods := at[sqlite3.Tsqlite3_file](inner(pFile)).FpMethods
	if innerMethods != 0 {
		table, err := methodTableFor(v, innerMethods)
		if err != nil {
			funcAt[ioCloseFunc](at[sqlite3.Tsqlite3_io_methods](innerMethods).FxClose)(tls, inner(pFile))
			v.setError(err)
			call.Result = codeCantOpen
			v.record(call)
			return codeCantOpen
		}
		registry.Lock()
		registry.files[pFile] = &openFile{vfs: v, path: path, name: displayName(path), kind: kind, realMethods: innerMethods}
		registry.Unlock()
		header.FpMethods = table
	}
	v.record(call)
	return rc
}

func vfsDelete(tls *libc.TLS, pVfs uintptr, zName uintptr, syncDir int32) int32 {
	rc := funcAt[vfsDeleteFunc](realVFS().FxDelete)(tls, registry.realVFS, zName, syncDir)
	vfsFor(pVfs).record(Call{Method: "xDelete", File: displayName(goString(zName)), Flags: int64(syncDir), Result: rc})
	return rc
}

func vfsAccess(tls *libc.TLS, pVfs uintptr, zName uintptr, flags int32, pResOut uintptr) int32 {
	rc := funcAt[vfsAccessFunc](realVFS().FxAccess)(tls, registry.realVFS, zName, flags, pResOut)
	vfsFor(pVfs).record(Call{Method: "xAccess", File: displayName(goString(zName)), Flags: int64(flags), Out: int64(*at[int32](pResOut)), Result: rc})
	return rc
}

func vfsFullPathname(tls *libc.TLS, pVfs uintptr, zName uintptr, nOut int32, zOut uintptr) int32 {
	rc := funcAt[vfsFullPathnameFunc](realVFS().FxFullPathname)(tls, registry.realVFS, zName, nOut, zOut)
	vfsFor(pVfs).record(Call{Method: "xFullPathname", File: displayName(goString(zName)), Result: rc})
	return rc
}

func vfsRandomness(tls *libc.TLS, pVfs uintptr, nByte int32, zOut uintptr) int32 {
	rc := funcAt[vfsRandomnessFunc](realVFS().FxRandomness)(tls, registry.realVFS, nByte, zOut)
	vfsFor(pVfs).record(Call{Method: "xRandomness", Amount: int64(nByte), Result: rc})
	return rc
}

func vfsSleep(tls *libc.TLS, pVfs uintptr, microseconds int32) int32 {
	rc := funcAt[vfsSleepFunc](realVFS().FxSleep)(tls, registry.realVFS, microseconds)
	vfsFor(pVfs).record(Call{Method: "xSleep", Amount: int64(microseconds), Result: rc})
	return rc
}

func vfsCurrentTime(tls *libc.TLS, pVfs uintptr, pOut uintptr) int32 {
	rc := funcAt[vfsCurrentTimeFunc](realVFS().FxCurrentTime)(tls, registry.realVFS, pOut)
	vfsFor(pVfs).record(Call{Method: "xCurrentTime", Result: rc})
	return rc
}

func vfsGetLastError(tls *libc.TLS, pVfs uintptr, nByte int32, zOut uintptr) int32 {
	return funcAt[vfsGetLastErrorFunc](realVFS().FxGetLastError)(tls, registry.realVFS, nByte, zOut)
}

func vfsCurrentTimeInt64(tls *libc.TLS, pVfs uintptr, pOut uintptr) int32 {
	rc := funcAt[vfsCurrentTimeInt64Func](realVFS().FxCurrentTimeInt64)(tls, registry.realVFS, pOut)
	vfsFor(pVfs).record(Call{Method: "xCurrentTimeInt64", Result: rc})
	return rc
}

func (f *openFile) call(method string) Call {
	return Call{Method: method, File: f.name, Kind: f.kind}
}

func ioClose(tls *libc.TLS, pFile uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xClose")
	call.Result = funcAt[ioCloseFunc](realMethods(f).FxClose)(tls, inner(pFile))
	registry.Lock()
	delete(registry.files, pFile)
	registry.Unlock()
	f.vfs.record(call)
	return call.Result
}

func ioRead(tls *libc.TLS, pFile uintptr, buf uintptr, amount int32, offset int64) int32 {
	f := fileFor(pFile)
	call := f.call("xRead")
	call.Amount, call.Offset = int64(amount), offset
	if code, ok := f.vfs.inject("xRead", f.kind); ok {
		return f.vfs.injected(call, code)
	}
	call.Result = funcAt[ioReadFunc](realMethods(f).FxRead)(tls, inner(pFile), buf, amount, offset)
	f.vfs.record(call)
	return call.Result
}

func ioWrite(tls *libc.TLS, pFile uintptr, buf uintptr, amount int32, offset int64) int32 {
	f := fileFor(pFile)
	call := f.call("xWrite")
	call.Amount, call.Offset = int64(amount), offset
	if code, ok := f.vfs.inject("xWrite", f.kind); ok {
		return f.vfs.injected(call, code)
	}
	call.Result = funcAt[ioWriteFunc](realMethods(f).FxWrite)(tls, inner(pFile), buf, amount, offset)
	f.vfs.record(call)
	return call.Result
}

func ioTruncate(tls *libc.TLS, pFile uintptr, size int64) int32 {
	f := fileFor(pFile)
	call := f.call("xTruncate")
	call.Amount = size
	if code, ok := f.vfs.inject("xTruncate", f.kind); ok {
		return f.vfs.injected(call, code)
	}
	call.Result = funcAt[ioTruncateFunc](realMethods(f).FxTruncate)(tls, inner(pFile), size)
	f.vfs.record(call)
	return call.Result
}

func ioSync(tls *libc.TLS, pFile uintptr, flags int32) int32 {
	f := fileFor(pFile)
	call := f.call("xSync")
	call.Flags = int64(flags)
	if code, ok := f.vfs.inject("xSync", f.kind); ok {
		return f.vfs.injected(call, code)
	}
	call.Result = funcAt[ioSyncFunc](realMethods(f).FxSync)(tls, inner(pFile), flags)
	f.vfs.record(call)
	return call.Result
}

func ioFileSize(tls *libc.TLS, pFile uintptr, pSize uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xFileSize")
	call.Result = funcAt[ioFileSizeFunc](realMethods(f).FxFileSize)(tls, inner(pFile), pSize)
	call.Out = *at[int64](pSize)
	f.vfs.record(call)
	return call.Result
}

func ioLock(tls *libc.TLS, pFile uintptr, level int32) int32 {
	f := fileFor(pFile)
	call := f.call("xLock")
	call.Flags = int64(level)
	call.Result = funcAt[ioLockFunc](realMethods(f).FxLock)(tls, inner(pFile), level)
	f.vfs.record(call)
	return call.Result
}

func ioUnlock(tls *libc.TLS, pFile uintptr, level int32) int32 {
	f := fileFor(pFile)
	call := f.call("xUnlock")
	call.Flags = int64(level)
	call.Result = funcAt[ioUnlockFunc](realMethods(f).FxUnlock)(tls, inner(pFile), level)
	f.vfs.record(call)
	return call.Result
}

func ioCheckReservedLock(tls *libc.TLS, pFile uintptr, pResOut uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xCheckReservedLock")
	call.Result = funcAt[ioCheckReservedLockFunc](realMethods(f).FxCheckReservedLock)(tls, inner(pFile), pResOut)
	call.Out = int64(*at[int32](pResOut))
	f.vfs.record(call)
	return call.Result
}

func ioFileControl(tls *libc.TLS, pFile uintptr, op int32, pArg uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xFileControl")
	call.Flags = int64(op)
	call.Result = funcAt[ioFileControlFunc](realMethods(f).FxFileControl)(tls, inner(pFile), op, pArg)
	f.vfs.record(call)
	return call.Result
}

func ioSectorSize(tls *libc.TLS, pFile uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xSectorSize")
	call.Result = funcAt[ioSectorSizeFunc](realMethods(f).FxSectorSize)(tls, inner(pFile))
	f.vfs.record(call)
	return call.Result
}

func ioDeviceCharacteristics(tls *libc.TLS, pFile uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xDeviceCharacteristics")
	call.Result = funcAt[ioDeviceCharacteristicsFunc](realMethods(f).FxDeviceCharacteristics)(tls, inner(pFile))
	f.vfs.record(call)
	return call.Result
}

func ioShmMap(tls *libc.TLS, pFile uintptr, region int32, size int32, extend int32, pp uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xShmMap")
	call.Offset, call.Amount, call.Flags = int64(region), int64(size), int64(extend)
	call.Result = funcAt[ioShmMapFunc](realMethods(f).FxShmMap)(tls, inner(pFile), region, size, extend, pp)
	f.vfs.record(call)
	return call.Result
}

func ioShmLock(tls *libc.TLS, pFile uintptr, offset int32, n int32, flags int32) int32 {
	f := fileFor(pFile)
	call := f.call("xShmLock")
	call.Offset, call.Amount, call.Flags = int64(offset), int64(n), int64(flags)
	call.Result = funcAt[ioShmLockFunc](realMethods(f).FxShmLock)(tls, inner(pFile), offset, n, flags)
	f.vfs.record(call)
	return call.Result
}

func ioShmBarrier(tls *libc.TLS, pFile uintptr) {
	f := fileFor(pFile)
	funcAt[ioShmBarrierFunc](realMethods(f).FxShmBarrier)(tls, inner(pFile))
	f.vfs.record(f.call("xShmBarrier"))
}

func ioShmUnmap(tls *libc.TLS, pFile uintptr, deleteFlag int32) int32 {
	f := fileFor(pFile)
	call := f.call("xShmUnmap")
	call.Flags = int64(deleteFlag)
	call.Result = funcAt[ioShmUnmapFunc](realMethods(f).FxShmUnmap)(tls, inner(pFile), deleteFlag)
	f.vfs.record(call)
	return call.Result
}

func ioFetch(tls *libc.TLS, pFile uintptr, offset int64, amount int32, pp uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xFetch")
	call.Offset, call.Amount = offset, int64(amount)
	call.Result = funcAt[ioFetchFunc](realMethods(f).FxFetch)(tls, inner(pFile), offset, amount, pp)
	f.vfs.record(call)
	return call.Result
}

func ioUnfetch(tls *libc.TLS, pFile uintptr, offset int64, p uintptr) int32 {
	f := fileFor(pFile)
	call := f.call("xUnfetch")
	call.Offset = offset
	call.Result = funcAt[ioUnfetchFunc](realMethods(f).FxUnfetch)(tls, inner(pFile), offset, p)
	f.vfs.record(call)
	return call.Result
}
