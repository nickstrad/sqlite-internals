package procs

import (
	"fmt"
	"os"
	"syscall"
)

// ProcLocksSupported reports that Linux /proc/locks inspection is available.
const ProcLocksSupported = true

// ReadProcLocks reads and parses all active kernel lock entries from /proc/locks.
func ReadProcLocks() ([]ProcLock, error) {
	data, err := os.ReadFile(ProcLocksPath)
	if err != nil {
		return nil, err
	}
	return ParseProcLocks(string(data))
}

// FileInode returns the filesystem inode number of path via stat(2).
func FileInode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("procs: no inode for %s", path)
	}
	return stat.Ino, nil
}
