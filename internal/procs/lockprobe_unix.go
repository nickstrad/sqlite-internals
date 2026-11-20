//go:build linux || darwin

package procs

import (
	"fmt"
	"os"
	"syscall"
)

const LockProbeSupported = true

func ProbeLock(path string, start, length int64) (LockHolder, error) {
	if start < 0 || length <= 0 {
		return LockHolder{}, fmt.Errorf("procs: probe range start %d length %d is not a byte range", start, length)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return LockHolder{}, err
	}
	defer file.Close()
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
