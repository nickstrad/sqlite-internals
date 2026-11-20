// Package dbfile parses SQLite 3 database files directly from raw bytes without
// calling SQLite.
//
// It decodes the 100-byte database header, B-tree pages and cells, overflow
// chains, records, sqlite_schema, the freelist, pointer-map pages, and the
// 1 GiB lock-byte page, and produces a complete per-page census. Every malformed
// input returns a typed FormatError rather than panicking.
package dbfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// PageCountSource values reported by File.PageCountSource.
const (
	// PageCountFromHeader means the page count came from header bytes 28-31.
	PageCountFromHeader = "in-header size"
	// PageCountFromFileSize means bytes 28-31 were 0 or stale, so file size / page size was used.
	PageCountFromFileSize = "file size"
)

// File provides read-only page and structure access to an SQLite database file.
type File struct {
	reader          io.ReaderAt
	closer          io.Closer
	size            int64
	Header          Header
	pageCount       int
	pageCountSource string
}

// Open opens an SQLite database file from disk and validates its header.
//
// Do not call Open on a file while the same process holds an open SQLite
// connection to it: closing any descriptor on a file drops all POSIX locks
// held by the process on that file.
func Open(path string) (*File, error) {
	osFile, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := osFile.Stat()
	if err != nil {
		osFile.Close()
		return nil, err
	}
	f, err := New(osFile, info.Size())
	if err != nil {
		osFile.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.closer = osFile
	return f, nil
}

// FromBytes wraps an in-memory database file image as a read-only File.
func FromBytes(image []byte) (*File, error) {
	return New(bytes.NewReader(image), int64(len(image)))
}

// New validates the header and page count of a database stored in reader of
// length size bytes.
func New(reader io.ReaderAt, size int64) (*File, error) {
	if size < HeaderSize {
		return nil, &FormatError{Kind: ErrTruncated, Offset: int(size), Problem: fmt.Sprintf("file has %d bytes, the header alone needs %d", size, HeaderSize)}
	}
	raw := make([]byte, HeaderSize)
	if _, err := reader.ReadAt(raw, 0); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	header, err := ParseHeader(raw)
	if err != nil {
		return nil, err
	}
	f := &File{reader: reader, size: size, Header: header}
	// Stricter than SQLite: SQLite rounds a partial trailing page up and zero-fills
	// it on read, but a page-based capture could not rebuild a partial page.
	if size%int64(header.PageSize) != 0 {
		return nil, &FormatError{Kind: ErrTruncated, Offset: int(size), Problem: fmt.Sprintf("file size %d is not a multiple of the page size %d", size, header.PageSize)}
	}
	filePages := size / int64(header.PageSize)
	if filePages > 0xfffffffe {
		return nil, &FormatError{Kind: ErrUnsupported, Offset: -1, Problem: fmt.Sprintf("file holds %d pages, more than a page number can address", filePages)}
	}
	f.pageCount = int(filePages)
	f.pageCountSource = PageCountFromFileSize
	// Mirror lockBtree (src/btree.c:3298-3302, 3405-3412): trust the in-header
	// page count only when non-zero and bytes 24-27 match 92-95, and treat an
	// in-header size larger than the file as corruption.
	if header.InHeaderSizeValid() {
		if int64(header.DatabaseSize) > filePages {
			return nil, &FormatError{Kind: ErrCorrupt, Offset: 28, Problem: fmt.Sprintf("in-header size %d pages is larger than the file (%d pages) while the change counters agree", header.DatabaseSize, filePages)}
		}
		f.pageCount = int(header.DatabaseSize)
		f.pageCountSource = PageCountFromHeader
	}
	return f, nil
}

// Close releases the underlying file descriptor if File was created by Open.
func (f *File) Close() error {
	if f.closer == nil {
		return nil
	}
	err := f.closer.Close()
	f.closer = nil
	return err
}

// FileSize returns the byte length of the underlying file or buffer.
func (f *File) FileSize() int64 { return f.size }

// PageSize returns the database page size in bytes.
func (f *File) PageSize() int { return f.Header.PageSize }

// UsableSize returns the usable byte count per page (page size minus reserved bytes).
func (f *File) UsableSize() int { return f.Header.UsableSize() }

// PageCount returns the effective number of pages in the database.
func (f *File) PageCount() int { return f.pageCount }

// PageCountSource reports whether PageCount came from PageCountFromHeader or
// PageCountFromFileSize.
func (f *File) PageCountSource() string { return f.pageCountSource }

// LockBytePage returns the 1-based lock-byte page number if this file is large
// enough to contain offset 1 GiB, or 0 otherwise.
func (f *File) LockBytePage() int {
	page := f.Header.LockBytePage()
	if page > f.pageCount {
		return 0
	}
	return page
}

func (f *File) checkPageNumber(n int) error {
	if n < 1 || n > f.pageCount {
		return &FormatError{Kind: ErrPageOutOfFile, Page: max(n, 0), Offset: -1, Problem: fmt.Sprintf("page %d is outside 1..%d", n, f.pageCount)}
	}
	return nil
}

// Page reads and returns a fresh copy of 1-based page n.
func (f *File) Page(n int) ([]byte, error) {
	if err := f.checkPageNumber(n); err != nil {
		return nil, err
	}
	data := make([]byte, f.Header.PageSize)
	read, err := f.reader.ReadAt(data, int64(n-1)*int64(f.Header.PageSize))
	if err != nil && !(errors.Is(err, io.EOF) && read == len(data)) {
		return nil, fmt.Errorf("read page %d: %w", n, err)
	}
	return data, nil
}

// PointerMapPage returns the pointer-map page governing page n when auto-vacuum
// is enabled, or 0 otherwise. Matches ptrmapPageno (src/btree.c:1036-1048): one
// pointer-map page holds usableSize/5 5-byte entries and therefore appears every
// usableSize/5 + 1 pages starting at page 2, shifted past the lock-byte page.
func (f *File) PointerMapPage(n int) int {
	if !f.Header.AutoVacuum() || n < 2 {
		return 0
	}
	perMap := f.UsableSize()/5 + 1
	mapPage := (n-2)/perMap*perMap + 2
	if mapPage == f.Header.LockBytePage() {
		mapPage++
	}
	return mapPage
}

// IsPointerMapPage reports whether page n is itself a pointer-map page.
func (f *File) IsPointerMapPage(n int) bool {
	return f.Header.AutoVacuum() && n >= 2 && n != f.Header.LockBytePage() && f.PointerMapPage(n) == n
}
