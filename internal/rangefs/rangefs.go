// Package rangefs implements a read-only io/fs.FS and fs.StatFS backed by
// byte-range reads over HTTP or S3, recording every stat and range request.
// Passed to modernc.org/sqlite/vfs.New, it lets SQLite query a checkpointed
// remote database snapshot one page miss at a time with no local copy, no
// caching, and no read-ahead.
package rangefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sync"
	"time"
)

// Source fetches file metadata and exact byte ranges from a remote store.
// Stat must return an error wrapping fs.ErrNotExist when the object is absent.
type Source interface {
	Stat(ctx context.Context, name string) (int64, error)
	ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error)
}

// RequestKind distinguishes metadata checks (stat) from byte-range reads (range).
type RequestKind string

// RequestKind values recorded by FS.
const (
	KindStat  RequestKind = "stat"
	KindRange RequestKind = "range"
)

// Request logs one remote operation issued through FS.
type Request struct {
	Kind   RequestKind
	Name   string
	Offset int64
	Length int64
	Found  bool
}

// String formats a Request as a single readable line for evidence output.
func (r Request) String() string {
	switch r.Kind {
	case KindStat:
		if r.Found {
			return fmt.Sprintf("stat %s (size %d)", r.Name, r.Length)
		}
		return fmt.Sprintf("stat %s (absent)", r.Name)
	case KindRange:
		return fmt.Sprintf("range %s bytes %d-%d (%d bytes)", r.Name, r.Offset, r.Offset+r.Length-1, r.Length)
	}
	return string(r.Kind)
}

// Stats aggregates request counts and total bytes read across a slice of Requests.
type Stats struct {
	StatRequests  int
	RangeRequests int
	BytesRead     int64
}

// Requests returns the total number of stat and range requests.
func (s Stats) Requests() int {
	return s.StatRequests + s.RangeRequests
}

// String formats Stats as "N requests (S stat, R range), B bytes".
func (s Stats) String() string {
	return fmt.Sprintf("%d requests (%d stat, %d range), %d bytes", s.Requests(), s.StatRequests, s.RangeRequests, s.BytesRead)
}

// Summarize tallies stat requests, range requests, and bytes read in requests.
func Summarize(requests []Request) Stats {
	var s Stats
	for _, r := range requests {
		switch r.Kind {
		case KindStat:
			s.StatRequests++
		case KindRange:
			s.RangeRequests++
			s.BytesRead += r.Length
		}
	}
	return s
}

// FS adapts a Source into a read-only fs.FS and fs.StatFS that records every
// remote request. Open and Stat each issue one Source.Stat call; each Read or
// ReadAt on an open file issues one Source.ReadRange call for the exact bytes
// requested, clipped at EOF.
type FS struct {
	source Source
	ctx    context.Context

	mu       sync.Mutex
	requests []Request
}

// New returns an FS that performs all Source calls using ctx.
func New(ctx context.Context, source Source) *FS {
	return &FS{source: source, ctx: ctx}
}

func (f *FS) log(r Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
}

// Requests returns a snapshot copy of all recorded requests.
func (f *FS) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// TakeRequests returns all recorded requests and clears the log.
func (f *FS) TakeRequests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests
	f.requests = nil
	return r
}

func (f *FS) stat(op, name string) (int64, error) {
	if !fs.ValidPath(name) {
		return 0, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	size, err := f.source.Stat(f.ctx, name)
	if err != nil {
		f.log(Request{Kind: KindStat, Name: name})
		return 0, &fs.PathError{Op: op, Path: name, Err: err}
	}
	f.log(Request{Kind: KindStat, Name: name, Length: size, Found: true})
	return size, nil
}

// Open stats name and returns a read-only seekable file implementing io.ReaderAt.
func (f *FS) Open(name string) (fs.File, error) {
	size, err := f.stat("open", name)
	if err != nil {
		return nil, err
	}
	return &file{fsys: f, name: name, size: size}, nil
}

// Stat implements fs.StatFS, which modernc.org/sqlite/vfs calls to answer
// SQLite's xAccess checks (for example, probing for -journal and -wal files).
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	size, err := f.stat("stat", name)
	if err != nil {
		return nil, err
	}
	return fileInfo{name: path.Base(name), size: size}, nil
}

type file struct {
	fsys   *FS
	name   string
	size   int64
	offset int64
	closed bool
}

func (f *file) Stat() (fs.FileInfo, error) {
	return fileInfo{name: path.Base(f.name), size: f.size}, nil
}

func (f *file) ReadAt(p []byte, offset int64) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if offset < 0 {
		return 0, fmt.Errorf("rangefs: negative offset %d", offset)
	}
	if offset >= f.size {
		return 0, io.EOF
	}
	// Clip the range at EOF so the store never receives an out-of-bounds
	// Range header, then return io.EOF on a short read as io.ReaderAt requires.
	want := min(int64(len(p)), f.size-offset)
	n, err := f.fsys.source.ReadRange(f.fsys.ctx, f.name, offset, p[:want])
	f.fsys.log(Request{Kind: KindRange, Name: f.name, Offset: offset, Length: want})
	if err != nil {
		return n, err
	}
	if int64(n) != want {
		return n, fmt.Errorf("rangefs: %s: short range read %d of %d bytes at %d", f.name, n, want, offset)
	}
	if want < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := f.ReadAt(p, f.offset)
	f.offset += int64(n)
	if err == io.EOF && n > 0 {
		return n, nil
	}
	return n, err
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.offset + offset
	case io.SeekEnd:
		next = f.size + offset
	default:
		return f.offset, fmt.Errorf("rangefs: bad whence %d", whence)
	}
	if next < 0 {
		return f.offset, errors.New("rangefs: seek before start of file")
	}
	f.offset = next
	return next, nil
}

func (f *file) Close() error {
	f.closed = true
	return nil
}

type fileInfo struct {
	name string
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return 0o444 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }
