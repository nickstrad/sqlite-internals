//go:build linux

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

const memorySource = "/proc/self/status"

func readProcessMemory() (processMemory, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return processMemory{}, err
	}
	defer f.Close()
	m := processMemory{RSS: -1, Anon: -1, File: -1, Shmem: -1}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "VmRSS:":
			m.RSS = kib * 1024
		case "RssAnon:":
			m.Anon = kib * 1024
		case "RssFile:":
			m.File = kib * 1024
		case "RssShmem:":
			m.Shmem = kib * 1024
		}
	}
	return m, scanner.Err()
}
