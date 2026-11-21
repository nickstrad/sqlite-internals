package walfile

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// TestMain intercepts LiveCopyChildMode when tests invoke CopyLiveFiles.
func TestMain(m *testing.M) {
	ServeLiveCopyChild(os.Args)
	os.Exit(m.Run())
}

func openLiveWAL(t *testing.T, path string) *sqlitex.DB {
	t.Helper()
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	options.Conn.WALAutocheckpoint = 0
	options.Conn.SecureDelete = true
	db, err := sqlitex.Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func execAll(t *testing.T, db *sqlitex.DB, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// TestLiveCopyReportsMissingFiles checks child-process copying, stale-target removal, and write errors.
func TestLiveCopyReportsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "a")
	if err := os.WriteFile(present, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale-copy")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := CopyLiveFiles(context.Background(), []CopyPair{{present, filepath.Join(dir, "b")}, {filepath.Join(dir, "none"), stale}})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Present || results[0].Bytes != 5 || results[1].Present {
		t.Fatalf("%+v", results)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a missing source must remove an old copy")
	}
	if _, err := CopyLiveFiles(context.Background(), []CopyPair{{present, filepath.Join(dir, "no-such-dir", "x")}}); err == nil {
		t.Fatal("an unwritable destination must be an error")
	}
}

// TestReplayMatchesSQLiteCheckpoint verifies live WAL/-shm parsing, showwal agreement, and E1 replay against TRUNCATE.
func TestReplayMatchesSQLiteCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	db := openLiveWAL(t, path)
	execAll(t, db,
		"CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB)",
		"INSERT INTO t VALUES (1, zeroblob(3000))",
		"INSERT INTO t VALUES (2, zeroblob(9000))",
		"UPDATE t SET body = zeroblob(100) WHERE id = 1",
		"DELETE FROM t WHERE id = 2",
	)
	copies := filepath.Join(dir, "copies")
	if err := os.Mkdir(copies, 0o755); err != nil {
		t.Fatal(err)
	}
	results, err := CopyLiveFiles(ctx, DatabaseFileSet(path, copies, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Present {
			t.Fatalf("%s missing", r.From)
		}
	}
	wal, err := ParseFile(filepath.Join(copies, "c.db-wal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(wal.Commits()) < 5 || wal.ValidFrames != len(wal.Frames) || wal.LastCommitFrame != wal.ValidFrames {
		t.Fatalf("commits=%d valid=%d frames=%d", len(wal.Commits()), wal.ValidFrames, len(wal.Frames))
	}
	if wal.Size != HeaderSize+int64(len(wal.Frames))*int64(4096+FrameHeaderSize) {
		t.Fatal("size formula")
	}
	index, err := ParseIndexFile(filepath.Join(copies, "c.db-shm"))
	if err != nil {
		t.Fatal(err)
	}
	h := index.Header()
	if !index.Initialized || !index.HeadersAgree() || !h.ChecksumOK || h.MaxFrame != uint32(wal.LastCommitFrame) || h.PageSize() != 4096 {
		t.Fatalf("index header %s", h)
	}
	if h.Salt1 != wal.Header.Salt1 || h.Salt2 != wal.Header.Salt2 || h.Pages != wal.DatabasePages() || index.Checkpoint.Backfill != 0 {
		t.Fatalf("index %s / %s", h, index.Checkpoint)
	}
	pages, err := index.FramePages(h.MaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pages {
		if wal.Frames[i].Page != p {
			t.Fatalf("frame %d: index page %d, WAL page %d", i+1, p, wal.Frames[i].Page)
		}
	}
	noop, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointNoop)
	if err != nil || noop.LogFrames != wal.ValidFrames {
		t.Fatalf("NOOP %s: %v", noop, err)
	}
	if showwal, err := sqlitex.LocateTool("showwal"); err == nil {
		out, err := exec.Command(showwal, filepath.Join(copies, "c.db-wal")).Output()
		if err != nil {
			t.Fatal(err)
		}
		listing, err := ParseShowwal(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if problems := wal.CompareWithShowwal(listing); len(problems) != 0 {
			t.Fatal(strings.Join(problems, "\n"))
		}
	}
	if result, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil || result.Busy {
		t.Fatalf("TRUNCATE %s: %v", result, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(filepath.Join(copies, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	walBytes, err := os.ReadFile(filepath.Join(copies, "c.db-wal"))
	if err != nil {
		t.Fatal(err)
	}
	got, report, err := Replay(base, walBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("replay differs from SQLite's checkpoint: %v", report.Lines())
	}
}

// TestReadLiveState checks ReadLiveState before open, after commits, and after TRUNCATE with temp cleanup.
func TestReadLiveState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	state, err := ReadLiveState(ctx, path, dir)
	if err != nil || state.WALPresent || state.IndexPresent {
		t.Fatalf("no database yet: %+v %v", state, err)
	}
	db := openLiveWAL(t, path)
	execAll(t, db, "CREATE TABLE t(x)", "INSERT INTO t VALUES (1)")
	state, err = ReadLiveState(ctx, path, dir)
	if err != nil || !state.WALPresent || !state.IndexPresent || state.WAL == nil || state.WALProblem != "" {
		t.Fatalf("%+v %v", state, err)
	}
	if state.Index.Header().MaxFrame != uint32(state.WAL.LastCommitFrame) || state.WALSize != state.WAL.Size {
		t.Fatalf("index %s, WAL last commit %d", state.Index.Header(), state.WAL.LastCommitFrame)
	}
	if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
		t.Fatal(err)
	}
	state, err = ReadLiveState(ctx, path, dir)
	if err != nil || !state.WALPresent || state.WALSize != 0 || len(state.WAL.Frames) != 0 || state.Index.Checkpoint.Backfill != 0 {
		t.Fatalf("after TRUNCATE: %+v %v", state, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "live-state-copy") {
			t.Fatalf("left behind %s", e.Name())
		}
	}
}
