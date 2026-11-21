//go:build linux

package main

import "syscall"

const rlimitSupported = true

func setNoFileLimit(limit uint64) error {
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: limit, Max: limit})
}
