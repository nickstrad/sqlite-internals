//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

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
