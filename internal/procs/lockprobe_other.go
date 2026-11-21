//go:build !linux && !darwin

package procs

import (
	"errors"
	"runtime"
)

// LockProbeSupported is false on platforms without a Linux/Darwin fcntl(F_GETLK) implementation.
const LockProbeSupported = false

// ProbeLock returns an unsupported-platform error on non-Linux, non-Darwin builds.
func ProbeLock(path string, start, length int64) (LockHolder, error) {
	return LockHolder{}, errors.New("procs: fcntl(F_GETLK) probing is implemented for linux and darwin, not " + runtime.GOOS)
}
