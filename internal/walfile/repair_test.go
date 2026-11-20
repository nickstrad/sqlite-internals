package walfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseIndexTornHeader(t *testing.T) {
	for _, bigEndian := range []bool{false, true} {
		x := testIndex{bigEndian: bigEndian, size: IndexBlockSize, header: IndexHeader{Version: IndexFormatVersion, IsInit: 1, PageSizeField: 0x1000, MaxFrame: 7, Pages: 3}}
		data := x.build()
		clear(data[:IndexHeaderSize])
		index, err := ParseIndex(data)
		if err != nil {
			t.Fatal(err)
		}
		if index.Initialized || index.HeadersAgree() || index.BigEndian != bigEndian || index.Headers[1].MaxFrame != 7 || !index.Headers[1].ChecksumOK || index.Headers[0].MaxFrame != 0 {
			t.Fatalf("torn header parsed as %+v", index)
		}
		if _, err := index.FramePage(1); !errors.Is(err, ErrFrameNotIndexed) {
			t.Fatal("a torn index must not answer frame lookups")
		}
	}
	data := make([]byte, IndexBlockSize)
	data[IndexHeaderSize] = 7
	if _, err := ParseIndex(data); !errors.Is(err, ErrIndexVersion) {
		t.Fatalf("garbage second header: %v", err)
	}
}

func TestHeadersAgreeComparesUnusedField(t *testing.T) {
	x := testIndex{size: IndexBlockSize, header: IndexHeader{Version: IndexFormatVersion, IsInit: 1, PageSizeField: 0x1000}}
	data := x.build()
	data[4] = 1
	data[IndexHeaderSize+4] = 2
	index, err := ParseIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	if index.Headers[0] != index.Headers[1] || index.HeadersAgree() {
		t.Fatal("copies that differ only in the unused field must not agree")
	}
}

func TestReplayValidatesBaseWithoutCommits(t *testing.T) {
	headerOnly8192 := testWAL{pageSize: 8192}.build()
	headerOnly1024 := testWAL{pageSize: 1024}.build()
	uncommitted1024 := testWAL{pageSize: 1024, frames: []testFrame{{page: 2, fill: 3}}}.build()
	if _, _, err := Replay(baseImage(1024, 2), headerOnly8192); !errors.Is(err, ErrPageSizeMismatch) {
		t.Fatalf("header-only page size mismatch: %v", err)
	}
	if _, _, err := Replay(append(baseImage(1024, 2), 1, 2, 3), headerOnly1024); !errors.Is(err, ErrBaseSize) {
		t.Fatalf("header-only base size: %v", err)
	}
	if _, _, err := Replay(append(baseImage(1024, 2), 1, 2, 3), uncommitted1024); !errors.Is(err, ErrBaseSize) {
		t.Fatalf("uncommitted base size: %v", err)
	}
	if _, _, err := Replay(append(baseImage(1024, 2), 1, 2, 3), nil); !errors.Is(err, ErrBaseSize) {
		t.Fatalf("empty WAL base size: %v", err)
	}
	for _, field := range [][2]byte{{0, 0}, {0x03, 0xe8}, {0x01, 0x00}} {
		base := baseImage(1024, 2)
		base[16], base[17] = field[0], field[1]
		if _, _, err := Replay(base, threeTransactions(false, 1024).build()); !errors.Is(err, ErrBaseNotDatabase) {
			t.Fatalf("page size field %v: %v", field, err)
		}
		if _, _, err := Replay(base, headerOnly1024); !errors.Is(err, ErrBaseNotDatabase) {
			t.Fatalf("page size field %v without commits: %v", field, err)
		}
	}
	out, _, err := Replay(baseImage(1024, 2), headerOnly1024)
	if err != nil || !bytes.Equal(out, baseImage(1024, 2)) {
		t.Fatalf("matching header-only WAL: %v", err)
	}
}

func TestReplayFilesKeepsForeignTempFile(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.db")
	if err := os.WriteFile(basePath, baseImage(1024, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.db")
	sentinel := []byte("not ours")
	if err := os.WriteFile(out+".replay-tmp", sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayFiles(basePath, filepath.Join(dir, "no-wal"), out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing temp file: %v", err)
	}
	got, err := os.ReadFile(out + ".replay-tmp")
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("temp file changed: %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no output may be written")
	}
}

func TestCompareWithShowwalReportsWrongRejection(t *testing.T) {
	file, err := Parse(threeTransactions(false, 512).build())
	if err != nil {
		t.Fatal(err)
	}
	listing := ShowwalListing{PageSize: 512, AvailableFrames: 6}
	for _, f := range file.Frames {
		listing.Frames = append(listing.Frames, ShowwalFrame{Number: f.Number, Page: f.Page, CommitSize: f.CommitSize, Salt1: f.Salt1, Salt2: f.Salt2, StoredChecksum: f.StoredChecksum, ChecksumAgrees: true})
	}
	file.ValidFrames = 2
	file.Frames[2].Problem = FrameChecksumMismatch
	if problems := file.CompareWithShowwal(listing); len(problems) != 1 {
		t.Fatal(problems)
	}
	listing.Frames[2].ChecksumAgrees = false
	if problems := file.CompareWithShowwal(listing); len(problems) != 0 {
		t.Fatal(problems)
	}
}

func TestCopyLiveFilesWithNoPairs(t *testing.T) {
	results, err := CopyLiveFiles(context.Background(), nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("%v %v", results, err)
	}
}

func TestReadLiveStateRemovesCopiesOnError(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "x.db")
	if err := os.WriteFile(database+"-shm", bytes.Repeat([]byte{7}, IndexBlockSize), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLiveState(context.Background(), database, scratch); !errors.Is(err, ErrIndexVersion) {
		t.Fatalf("garbage index: %v", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left behind %v (%v)", entries, err)
	}
}
