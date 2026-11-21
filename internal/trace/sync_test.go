package trace

import "testing"

// TestSyncsOf verifies sync counting, post-sync write tracking, path filtering, failed/unfinished call handling, and ftruncate offset calculation.
func TestSyncsOf(t *testing.T) {
	calls := []Syscall{
		{Name: "pwrite64", Path: "/d/x-wal", Offset: 0, Length: 32, Result: 32},
		{Name: "fsync", Path: "/d/x-wal"},
		{Name: "pwrite64", Path: "/d/x-wal", Offset: 32, Length: 24, Result: 24},
		{Name: "pwrite64", Path: "/d/x", Offset: 4096, Length: 4096, Result: 4096},
		{Name: "fsync", Path: "/d/x-wal"},
		{Name: "pwrite64", Path: "/d/x-wal", Offset: 4152, Length: 24, Result: 24},
		{Name: "pwrite64", Path: "/d/x-wal", Offset: 4176, Length: 4096, Result: 4096},
		{Name: "pwrite64", Path: "/d/x-wal", Offset: 9000, Length: 10, Result: -1, Errno: "EIO"},
		{Name: "fsync", Path: "/d/other"},
		{Name: "fsync", Path: "/d/x-wal", Unfinished: true},
	}
	wal := SyncsOf(calls, "/d/x-wal")
	if wal.Syncs != 2 || wal.LastSync != 4 || len(wal.Unsynced) != 2 || wal.FirstUnsynced != 4152 || !wal.HasUnsynced || !wal.UnfinishedSeen {
		t.Fatalf("wal %+v", wal)
	}
	db := SyncsOf(calls, "/d/x")
	if db.Syncs != 0 || db.LastSync != -1 || len(db.Unsynced) != 1 || db.FirstUnsynced != 4096 {
		t.Fatalf("db %+v", db)
	}
	truncated := SyncsOf([]Syscall{{Name: "pwrite64", Path: "/p", Offset: 8192, Length: 1, Result: 1}, {Name: "ftruncate", Path: "/p", Length: 4096}}, "/p")
	if truncated.FirstUnsynced != 4096 {
		t.Fatalf("ftruncate %+v", truncated)
	}
	clean := SyncsOf(calls[:2], "/d/x-wal")
	if clean.HasUnsynced || clean.String() != "1 sync(s); no write after the last one" {
		t.Fatalf("clean %+v %q", clean, clean.String())
	}
	if wal.String() != "2 sync(s); 2 write(s) after the last one, the lowest at offset 4152" {
		t.Fatalf("%q", wal.String())
	}
}
