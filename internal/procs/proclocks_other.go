//go:build !linux

package procs

import (
	"errors"
	"runtime"
)

// ProcLocksSupported is false on non-Linux operating systems where /proc/locks does not exist.
const ProcLocksSupported = false

// ErrNoProcLocks is returned by ReadProcLocks and FileInode on non-Linux builds.
var ErrNoProcLocks = errors.New("procs: /proc/locks exists only on Linux, not on " + runtime.GOOS)

// ReadProcLocks returns ErrNoProcLocks on non-Linux builds.
func ReadProcLocks() ([]ProcLock, error) { return nil, ErrNoProcLocks }

// FileInode returns ErrNoProcLocks on non-Linux builds.
func FileInode(path string) (uint64, error) { return 0, ErrNoProcLocks }
