package main

import (
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/trace"
)

func parsed(t *testing.T, lines ...string) []trace.VFSCall {
	t.Helper()
	var calls []trace.VFSCall
	for _, line := range lines {
		c, ok := trace.ParseVFSLine(line)
		if !ok {
			t.Fatalf("unparsed: %s", line)
		}
		calls = append(calls, c)
	}
	return calls
}

func TestCloseLifecycleMatchesRealVFSTraceLines(t *testing.T) {
	calls := parsed(t,
		`trace.xOpen(t.db-wal,flags=0x80006) -> SQLITE_OK`,
		`trace.xShmUnmap(t.db,delFlag=1) -> SQLITE_OK`,
		`trace.xDelete("/work/7.1/t.db-wal",0) -> SQLITE_OK`,
	)
	walOpen, walDelete, shmDelete := closeLifecycle(calls)
	if !walOpen || !walDelete || !shmDelete {
		t.Fatalf("walOpen=%t walDelete=%t shmDelete=%t calls=%+v", walOpen, walDelete, shmDelete, calls)
	}
}

func TestCloseLifecycleRejectsKeptShm(t *testing.T) {
	_, _, shmDelete := closeLifecycle(parsed(t, `trace.xShmUnmap(t.db,delFlag=0) -> SQLITE_OK`))
	if shmDelete {
		t.Fatal("delFlag=0 counted as delete")
	}
}
