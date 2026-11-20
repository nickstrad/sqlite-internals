//go:build !linux && !darwin

package procs

import (
	"errors"
	"runtime"
)

const LockProbeSupported = false

func ProbeLock(path string, start, length int64) (LockHolder, error) {
	return LockHolder{}, errors.New("procs: fcntl(F_GETLK) probing is implemented for linux and darwin, not " + runtime.GOOS)
}
