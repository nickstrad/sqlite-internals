package dbfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	PageCountFromHeader   = "in-header size"
	PageCountFromFileSize = "file size"
)

type File struct {
	reader          io.ReaderAt
	closer          io.Closer
	size            int64
	Header          Header
	pageCount       int
	pageCountSource string
}

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

func FromBytes(image []byte) (*File, error) {
	return New(bytes.NewReader(image), int64(len(image)))
}

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
	if size%int64(header.PageSize) != 0 {
		return nil, &FormatError{Kind: ErrTruncated, Offset: int(size), Problem: fmt.Sprintf("file size %d is not a multiple of the page size %d", size, header.PageSize)}
	}
	filePages := size / int64(header.PageSize)
	if filePages > 0xfffffffe {
		return nil, &FormatError{Kind: ErrUnsupported, Offset: -1, Problem: fmt.Sprintf("file holds %d pages, more than a page number can address", filePages)}
	}
	f.pageCount = int(filePages)
	f.pageCountSource = PageCountFromFileSize
	if header.InHeaderSizeValid() {
		if int64(header.DatabaseSize) > filePages {
			return nil, &FormatError{Kind: ErrCorrupt, Offset: 28, Problem: fmt.Sprintf("in-header size %d pages is larger than the file (%d pages) while the change counters agree", header.DatabaseSize, filePages)}
		}
		f.pageCount = int(header.DatabaseSize)
		f.pageCountSource = PageCountFromHeader
	}
	return f, nil
}

func (f *File) Close() error {
	if f.closer == nil {
		return nil
	}
	err := f.closer.Close()
	f.closer = nil
	return err
}

func (f *File) FileSize() int64         { return f.size }
func (f *File) PageSize() int           { return f.Header.PageSize }
func (f *File) UsableSize() int         { return f.Header.UsableSize() }
func (f *File) PageCount() int          { return f.pageCount }
func (f *File) PageCountSource() string { return f.pageCountSource }

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

func (f *File) IsPointerMapPage(n int) bool {
	return f.Header.AutoVacuum() && n >= 2 && n != f.Header.LockBytePage() && f.PointerMapPage(n) == n
}
