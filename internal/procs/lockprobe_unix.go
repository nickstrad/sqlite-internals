//go:build linux || darwin

package procs

import (
	"fmt"
	"os"
	"syscall"
)

// LockProbeSupported reports that fcntl(F_GETLK) lock probing is available on this OS.
const LockProbeSupported = true

// ProbeLock asks the kernel via fcntl(F_GETLK) whether another process holds a
// POSIX advisory lock conflicting with a write lock on [start, start+length) of
// path (the same mechanism SQLite uses in unixIsSharingShmNode, src/os_unix.c:4679-4695).
// Must be called from a process that holds no SQLite connection to path, because
// closing the temporary file descriptor releases all of the caller's POSIX locks
// on that inode (src/os_unix.c:1246-1248).
func ProbeLock(path string, start, length int64) (LockHolder, error) {
	if start < 0 || length <= 0 {
		return LockHolder{}, fmt.Errorf("procs: probe range start %d length %d is not a byte range", start, length)
	}
	// F_WRLCK probes require a writable descriptor on POSIX systems.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return LockHolder{}, err
	}
	defer file.Close()
	// Probing with F_WRLCK conflicts with both F_RDLCK and F_WRLCK held by other processes.
	probe := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: start, Len: length}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &probe); err != nil {
		return LockHolder{}, fmt.Errorf("fcntl(F_GETLK) on %s: %w", path, err)
	}
	switch probe.Type {
	case syscall.F_UNLCK:
		return LockHolder{}, nil
	case syscall.F_RDLCK, syscall.F_WRLCK:
		return LockHolder{Held: true, Write: probe.Type == syscall.F_WRLCK, Pid: int(probe.Pid), Start: probe.Start, Length: probe.Len}, nil
	}
	return LockHolder{}, fmt.Errorf("fcntl(F_GETLK) on %s returned lock type %d", path, probe.Type)
}
