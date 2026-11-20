package rangefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type stubSource struct {
	data    []byte
	readErr error
	short   int
	reads   int
}

func (s *stubSource) Stat(ctx context.Context, name string) (int64, error) {
	if name != "a.db" {
		return 0, fs.ErrNotExist
	}
	return int64(len(s.data)), nil
}

func (s *stubSource) ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.reads++
	if s.readErr != nil {
		return 0, s.readErr
	}
	n := copy(p, s.data[offset:])
	if s.short > 0 && s.short < n {
		n = s.short
	}
	return n, nil
}

func openStub(t *testing.T, src *stubSource) (*FS, fs.File) {
	t.Helper()
	fsys := New(context.Background(), src)
	f, err := fsys.Open("a.db")
	if err != nil {
		t.Fatal(err)
	}
	fsys.TakeRequests()
	return fsys, f
}

func TestZeroLengthReadAtLogsNothing(t *testing.T) {
	src := &stubSource{data: make([]byte, 100)}
	fsys, f := openStub(t, src)
	n, err := f.(io.ReaderAt).ReadAt(nil, 10)
	if n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if reqs := fsys.Requests(); len(reqs) != 0 || src.reads != 0 {
		t.Fatalf("requests=%v reads=%d", reqs, src.reads)
	}
}

func TestFailedRangeCountsNoBytes(t *testing.T) {
	src := &stubSource{data: make([]byte, 8192), readErr: errors.New("boom")}
	fsys, f := openStub(t, src)
	if _, err := f.(io.ReaderAt).ReadAt(make([]byte, 4096), 0); err == nil {
		t.Fatal("want error")
	}
	reqs := fsys.Requests()
	st := Summarize(reqs)
	if st.RangeRequests != 1 || st.BytesRead != 0 || !reqs[0].Failed {
		t.Fatalf("%+v %v", st, reqs)
	}
	if got := reqs[0].String(); got != "range a.db at 0 failed (0 bytes received)" {
		t.Fatal(got)
	}
}

func TestShortRangeCountsReceivedBytes(t *testing.T) {
	src := &stubSource{data: make([]byte, 8192), short: 100}
	fsys, f := openStub(t, src)
	if _, err := f.(io.ReaderAt).ReadAt(make([]byte, 4096), 0); err == nil {
		t.Fatal("want short-read error")
	}
	st := Summarize(fsys.Requests())
	if st.RangeRequests != 1 || st.BytesRead != 100 {
		t.Fatalf("%+v", st)
	}
}

func TestSeekAndStatAfterClose(t *testing.T) {
	_, f := openStub(t, &stubSource{data: make([]byte, 10)})
	f.Close()
	if _, err := f.(io.Seeker).Seek(0, io.SeekStart); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Stat(); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("stat: %v", err)
	}
}

func TestNilContextDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	var ctx context.Context
	fsys := New(ctx, HTTPSource{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := fsys.Stat("a.db"); err != nil {
		t.Fatal(err)
	}
}

func hostileS3(t *testing.T, contentRange string, status int) S3Source {
	t.Helper()
	obj := make([]byte, 8192)
	for i := 4096; i < len(obj); i++ {
		obj[i] = 0xbb
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := obj
		if contentRange != "" {
			w.Header().Set("Content-Range", contentRange)
			body = obj[4096:]
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return S3Source{Client: NewS3Client(S3Config{Endpoint: srv.URL, Region: "us-east-1", AccessKey: "k", SecretKey: "s"}), Bucket: "b"}
}

func TestS3RejectsIgnoredRange(t *testing.T) {
	src := hostileS3(t, "", http.StatusOK)
	n, err := src.ReadRange(context.Background(), "a.db", 4096, make([]byte, 4096))
	if err == nil || n != 0 || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestS3RejectsWrongContentRange(t *testing.T) {
	src := hostileS3(t, "bytes 0-4095/8192", http.StatusPartialContent)
	if _, err := src.ReadRange(context.Background(), "a.db", 4096, make([]byte, 4096)); err == nil {
		t.Fatal("wrong Content-Range accepted")
	}
}

func TestS3AcceptsMatchingContentRange(t *testing.T) {
	src := hostileS3(t, "bytes 4096-8191/8192", http.StatusPartialContent)
	buf := make([]byte, 4096)
	n, err := src.ReadRange(context.Background(), "a.db", 4096, buf)
	if err != nil || n != 4096 || buf[0] != 0xbb {
		t.Fatalf("n=%d err=%v b=%x", n, err, buf[0])
	}
}
