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

type Source interface {
	Stat(ctx context.Context, name string) (int64, error)
	ReadRange(ctx context.Context, name string, offset int64, p []byte) (int, error)
}

type RequestKind string

const (
	KindStat  RequestKind = "stat"
	KindRange RequestKind = "range"
)

type Request struct {
	Kind   RequestKind
	Name   string
	Offset int64
	Length int64
	Found  bool
}

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

type Stats struct {
	StatRequests  int
	RangeRequests int
	BytesRead     int64
}

func (s Stats) Requests() int {
	return s.StatRequests + s.RangeRequests
}

func (s Stats) String() string {
	return fmt.Sprintf("%d requests (%d stat, %d range), %d bytes", s.Requests(), s.StatRequests, s.RangeRequests, s.BytesRead)
}

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

type FS struct {
	source Source
	ctx    context.Context

	mu       sync.Mutex
	requests []Request
}

func New(ctx context.Context, source Source) *FS {
	return &FS{source: source, ctx: ctx}
}

func (f *FS) log(r Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
}

func (f *FS) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

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

func (f *FS) Open(name string) (fs.File, error) {
	size, err := f.stat("open", name)
	if err != nil {
		return nil, err
	}
	return &file{fsys: f, name: name, size: size}, nil
}

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
