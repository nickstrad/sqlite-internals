//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// diskUsage reports the actual disk blocks allocated for path on Unix systems
// (blocks are counted in 512-byte units in syscall.Stat_t), demonstrating that
// the > 1 GiB test file in part E is sparse.
func diskUsage(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errNoUsage.Error()
	}
	return fmt.Sprintf("%d bytes allocated for a %d-byte file", stat.Blocks*512, info.Size())
}
