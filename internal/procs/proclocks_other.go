//go:build !linux

package procs

import (
	"errors"
	"runtime"
)

const ProcLocksSupported = false

var ErrNoProcLocks = errors.New("procs: /proc/locks exists only on Linux, not on " + runtime.GOOS)

func ReadProcLocks() ([]ProcLock, error) { return nil, ErrNoProcLocks }

func FileInode(path string) (uint64, error) { return 0, ErrNoProcLocks }
