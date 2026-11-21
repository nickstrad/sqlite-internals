package main

import (
	"slices"
	"testing"

	"github.com/nickstrad/walfleet/internal/trace"
)

// TestNormalizedKeepsErrorsLengthsAndShortTransfers checks that normalized
// preserves errno values, read/write lengths, and short-transfer byte counts
// so failed or partial syscalls never compare equal to successful ones.
func TestNormalizedKeepsErrorsLengthsAndShortTransfers(t *testing.T) {
	cases := []struct {
		call trace.Syscall
		want string
	}{
		{trace.Syscall{Name: "openat", Path: "t.db-journal", Errno: "ENOENT", Result: -1}, "open t.db-journal = ENOENT"},
		{trace.Syscall{Name: "pwrite64", Path: "t.db", Offset: 0, Length: 4096, Result: 4096}, "pwrite64 t.db off=0 len=4096"},
		{trace.Syscall{Name: "pwrite64", Path: "t.db", Offset: 0, Length: 4096, Result: -1, Errno: "EIO"}, "pwrite64 t.db off=0 len=4096 = EIO"},
		{trace.Syscall{Name: "pread64", Path: "t.db-journal", Offset: 9216, Length: 8, Result: 0}, "pread64 t.db-journal off=9216 len=8 = 0"},
		{trace.Syscall{Name: "write", Path: "t.db", FD: 3, Offset: -1, Length: 100, Result: 100}, "write t.db len=100"},
		{trace.Syscall{Name: "read", Path: "t.db", FD: 3, Offset: -1, Length: 100, Result: 40}, "read t.db len=100 = 40"},
		{trace.Syscall{Name: "fdatasync", Path: "t.db-wal", Result: -1, Errno: "EIO"}, "sync t.db-wal = EIO"},
		{trace.Syscall{Name: "unlinkat", Path: "t.db-journal", Result: -1, Errno: "ENOENT"}, "unlink t.db-journal = ENOENT"},
		{trace.Syscall{Name: "ftruncate", Path: "t.db", Length: 0, Result: -1, Errno: "EINVAL"}, "ftruncate t.db len=0 = EINVAL"},
		{trace.Syscall{Name: "fcntl", Path: "t.db", Flags: "F_SETFD", Result: -1, Errno: "EBADF"}, "fcntl t.db F_SETFD = EBADF"},
	}
	for _, c := range cases {
		if got := normalized(c.call); got != c.want {
			t.Errorf("normalized(%+v) = %q, want %q", c.call, got, c.want)
		}
	}
	ok := trace.Syscall{Name: "fsync", Path: "t.db"}
	failed := trace.Syscall{Name: "fsync", Path: "t.db", Result: -1, Errno: "EIO"}
	if normalized(ok) == normalized(failed) {
		t.Fatal("a failed sync normalizes like a successful one")
	}
}

// TestExpectedFreshWALWrites checks the full offset+length sequence for a new
// WAL file and verifies that missing or unexpected extra writes fail to match.
func TestExpectedFreshWALWrites(t *testing.T) {
	frames := []walFrame{{Index: 1, Page: 2, CommitSize: 0}, {Index: 2, Page: 1, CommitSize: 2}}
	want := []string{"0+32", "32+24", "56+4096", "4152+24", "4176+4096"}
	if got := expectedFreshWALWrites(frames); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := expectedFreshWALWrites(nil); got != nil {
		t.Fatalf("no frames: got %v", got)
	}
	tooFew := []string{"32+24"}
	if slices.Equal(tooFew, expectedFreshWALWrites(frames)) {
		t.Fatal("fewer writes than frames must not match")
	}
	extraBefore := append([]string{"8192+4096"}, want...)
	if slices.Equal(extraBefore, expectedFreshWALWrites(frames)) {
		t.Fatal("an unexpected write before the frames must not match")
	}
}

// TestExpectedChangedPages checks the expected modified page lists for WAL and
// rollback modes.
func TestExpectedChangedPages(t *testing.T) {
	if !slices.Equal(expectedChangedPages("wal"), []int{2}) || !slices.Equal(expectedChangedPages("delete"), []int{1, 2}) {
		t.Fatal("expected changed pages")
	}
}
