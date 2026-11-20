package procs

import (
	"fmt"
	"os"
	"syscall"
)

const ProcLocksSupported = true

func ReadProcLocks() ([]ProcLock, error) {
	data, err := os.ReadFile(ProcLocksPath)
	if err != nil {
		return nil, err
	}
	return ParseProcLocks(string(data))
}

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
