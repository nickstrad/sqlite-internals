//go:build !linux

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const memorySource = "ps -o rss= -p <pid>"

func readProcessMemory() (processMemory, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return processMemory{}, err
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return processMemory{}, err
	}
	return processMemory{RSS: kib * 1024, Anon: -1, File: -1, Shmem: -1}, nil
}
