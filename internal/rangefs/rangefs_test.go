package rangefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func serve(t *testing.T, files map[string][]byte) *FS {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(server.Close)
	return New(context.Background(), HTTPSource{BaseURL: server.URL, Client: server.Client()})
}

// TestHTTPRangeReads verifies range reads, EOF clipping, request logging, and Summarize over HTTPSource.
func TestHTTPRangeReads(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 1000)
	fsys := serve(t, map[string][]byte{"a.db": body})
	f, err := fsys.Open("a.db")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != int64(len(body)) || info.Name() != "a.db" {
		t.Fatalf("stat %v %v", info, err)
	}
	seeker := f.(io.Seeker)
	if _, err := seeker.Seek(4096, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	page := make([]byte, 100)
	if _, err := io.ReadFull(f, page); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page, body[4096:4196]) {
		t.Fatal("wrong bytes")
	}
	tail := make([]byte, 50)
	n, err := f.(io.ReaderAt).ReadAt(tail, int64(len(body))-20)
	if n != 20 || err != io.EOF || !bytes.Equal(tail[:20], body[len(body)-20:]) {
		t.Fatalf("tail read n=%d err=%v", n, err)
	}
	if _, err := f.(io.ReaderAt).ReadAt(tail, int64(len(body))); err != io.EOF {
		t.Fatalf("read at end: %v", err)
	}
	requests := fsys.TakeRequests()
	want := []string{"stat a.db (size 10000)", "range a.db bytes 4096-4195 (100 bytes)", "range a.db bytes 9980-9999 (20 bytes)"}
	if len(requests) != len(want) {
		t.Fatalf("requests %v", requests)
	}
	for i, r := range requests {
		if r.String() != want[i] {
			t.Errorf("request %d: %q want %q", i, r, want[i])
		}
	}
	if s := Summarize(requests); s.Requests() != 3 || s.BytesRead != 120 || s.String() != "3 requests (1 stat, 2 range), 120 bytes" {
		t.Fatal(s)
	}
	if len(fsys.Requests()) != 0 {
		t.Fatal("TakeRequests did not clear")
	}
}

// TestMissingAndInvalid verifies 404 maps to fs.ErrNotExist, invalid paths return fs.ErrInvalid, and Stat implements fs.StatFS.
func TestMissingAndInvalid(t *testing.T) {
	fsys := serve(t, map[string][]byte{"a.db": []byte("x")})
	if _, err := fsys.Open("a.db-wal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := fs.Stat(fsys, "a.db-journal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat missing: %v", err)
	}
	if _, err := fsys.Open("/abs"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("invalid: %v", err)
	}
	info, err := fs.Stat(fsys, "a.db")
	if err != nil || info.Size() != 1 || info.IsDir() || info.Mode() != 0o444 || !info.ModTime().Equal(time.Time{}) || info.Sys() != nil {
		t.Fatalf("stat %v %v", info, err)
	}
	if got := fsys.Requests()[0].String(); got != "stat a.db-wal (absent)" {
		t.Fatal(got)
	}
}

// TestSeekAndClose verifies Seek whence modes, negative-offset rejection, and fs.ErrClosed after Close.
func TestSeekAndClose(t *testing.T) {
	fsys := serve(t, map[string][]byte{"a.db": []byte("abcdef")})
	f, err := fsys.Open("a.db")
	if err != nil {
		t.Fatal(err)
	}
	s := f.(io.Seeker)
	if pos, _ := s.Seek(-2, io.SeekEnd); pos != 4 {
		t.Fatal(pos)
	}
	if pos, _ := s.Seek(-1, io.SeekCurrent); pos != 3 {
		t.Fatal(pos)
	}
	if _, err := s.Seek(-10, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	rest, err := io.ReadAll(f)
	if err != nil || string(rest) != "def" {
		t.Fatalf("%q %v", rest, err)
	}
	f.Close()
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
}

// TestS3ConfigFromEnv verifies environment overrides, fallback defaults, and S3Source prefix joining.
func TestS3ConfigFromEnv(t *testing.T) {
	t.Setenv("COURSE_S3_ENDPOINT", "")
	t.Setenv("COURSE_S3_ACCESS_KEY", "k")
	cfg := S3ConfigFromEnv()
	if cfg.Endpoint != DefaultS3Endpoint || cfg.AccessKey != "k" || cfg.Region != DefaultS3Region || cfg.SecretKey != DefaultS3SecretKey {
		t.Fatalf("%+v", cfg)
	}
	if (S3Source{Prefix: "a/b/"}).key("x.db") != "a/b/x.db" || (S3Source{}).key("x.db") != "x.db" {
		t.Fatal("key")
	}
}

// TestS3RoundTripLive exercises PutObject, range reads, and missing-object lookup against a live SeaweedFS store when reachable.
func TestS3RoundTripLive(t *testing.T) {
	cfg := S3ConfigFromEnv()
	client := NewS3Client(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bucket := "course"
	if err := EnsureBucket(ctx, client, bucket); err != nil {
		t.Skipf("S3 store at %s not reachable: %v", cfg.Endpoint, err)
	}
	key := "rangefs-test/a.db"
	body := bytes.Repeat([]byte("s3"), 5000)
	if err := PutObject(ctx, client, bucket, key, body); err != nil {
		t.Fatal(err)
	}
	defer DeleteObject(context.Background(), client, bucket, key)
	fsys := New(ctx, S3Source{Client: client, Bucket: bucket, Prefix: "rangefs-test"})
	f, err := fsys.Open("a.db")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 10)
	if _, err := f.(io.ReaderAt).ReadAt(buf, 9990); err != nil || !bytes.Equal(buf, body[9990:]) {
		t.Fatalf("%q %v", buf, err)
	}
	if _, err := fsys.Open("missing.db"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}
