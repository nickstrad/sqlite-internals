package main

import (
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/trace"
)

// TestCountsSkipFailedSyscalls verifies that failed syscalls (such as ENOENT on
// unlink/open) are ignored while successful syscalls are tallied normally.
func TestCountsSkipFailedSyscalls(t *testing.T) {
	c := newCounts()
	c.add(trace.Syscall{Name: "unlink", Path: dbName + "-journal", Result: -1, Errno: "ENOENT"})
	c.add(trace.Syscall{Name: "openat", Path: dbName + "-journal", Result: -1, Errno: "ENOENT"})
	c.add(trace.Syscall{Name: "fsync", Path: dbName, Result: -1, Errno: "EIO"})
	c.add(trace.Syscall{Name: "ftruncate", Path: dbName + "-wal", Result: -1, Errno: "EINVAL"})
	if len(c.unlinks)+len(c.opens)+len(c.syncs)+len(c.truncates) != 0 {
		t.Fatalf("failed syscalls counted: %+v", c)
	}
	c.add(trace.Syscall{Name: "fdatasync", Path: dbName})
	if c.syncs["db"] != 1 {
		t.Fatalf("%+v", c.syncs)
	}
}

// TestClassifyWALCommits verifies that WAL commit validation requires at least
// one checkpoint followed by a matching restart and rejects unknown shapes.
func TestClassifyWALCommits(t *testing.T) {
	plain := commitShape{syncs: "wal:1"}
	checkpoint := commitShape{syncs: "db:1 wal:2", dbWrites: true}
	restart := commitShape{syncs: "wal:2"}
	classify := func(seq ...commitShape) (bool, int, int) {
		return classifyWALCommits([][]commitShape{seq}, "wal:1", "wal:2", "db:1 wal:2")
	}
	if ok, cp, rs := classify(plain, checkpoint, restart, plain); !ok || cp != 1 || rs != 1 {
		t.Fatalf("ok=%t cp=%d rs=%d", ok, cp, rs)
	}
	if ok, _, _ := classify(plain, plain, plain); ok {
		t.Fatal("no checkpoint accepted")
	}
	if ok, _, _ := classify(plain, checkpoint); ok {
		t.Fatal("checkpoint without restart accepted")
	}
	if ok, _, _ := classify(plain, commitShape{syncs: "db:9"}); ok {
		t.Fatal("unknown shape accepted")
	}
}
