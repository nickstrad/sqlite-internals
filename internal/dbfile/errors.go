package dbfile

import (
	"errors"
	"fmt"
)

// Sentinel errors returned inside FormatError so callers can classify malformed
// or truncated files with errors.Is.
var (
	// ErrNotADatabase reports an invalid magic string or header field value.
	ErrNotADatabase = errors.New("not an SQLite database file")
	// ErrTruncated reports a file shorter than the header or not a whole number of pages.
	ErrTruncated = errors.New("file is truncated")
	// ErrCorrupt reports an internal structural error inside the file, a page, a chain, or a tree.
	ErrCorrupt = errors.New("database file is malformed")
	// ErrUnsupported reports a valid format value this parser does not handle (for example, > 2^32-2 pages).
	ErrUnsupported = errors.New("unsupported database file feature")
	// ErrPageOutOfFile reports a page request outside 1..PageCount.
	ErrPageOutOfFile = errors.New("page number outside the file")
)

// FormatError describes a file-format violation along with the 1-based page
// number (0 for file-level errors) and byte offset (-1 when not byte-specific).
type FormatError struct {
	Kind    error
	Page    int
	Offset  int
	Problem string
}

// Error formats the error kind, page or file offset, and problem description.
func (e *FormatError) Error() string {
	location := "file"
	if e.Page > 0 {
		location = fmt.Sprintf("page %d", e.Page)
		if e.Offset >= 0 {
			location += fmt.Sprintf(" offset %d", e.Offset)
		}
	} else if e.Offset >= 0 {
		location = fmt.Sprintf("file offset %d", e.Offset)
	}
	return fmt.Sprintf("%s: %s: %s", e.Kind, location, e.Problem)
}

// Unwrap returns the underlying sentinel error (Kind) for errors.Is matching.
func (e *FormatError) Unwrap() error { return e.Kind }

func corruptAt(page, offset int, format string, args ...any) error {
	return &FormatError{Kind: ErrCorrupt, Page: page, Offset: offset, Problem: fmt.Sprintf(format, args...)}
}

func corruptPage(page int, format string, args ...any) error {
	return corruptAt(page, -1, format, args...)
}
