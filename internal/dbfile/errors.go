package dbfile

import (
	"errors"
	"fmt"
)

var (
	ErrNotADatabase  = errors.New("not an SQLite database file")
	ErrTruncated     = errors.New("file is truncated")
	ErrCorrupt       = errors.New("database file is malformed")
	ErrUnsupported   = errors.New("unsupported database file feature")
	ErrPageOutOfFile = errors.New("page number outside the file")
)

type FormatError struct {
	Kind    error
	Page    int
	Offset  int
	Problem string
}

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

func (e *FormatError) Unwrap() error { return e.Kind }

func corruptAt(page, offset int, format string, args ...any) error {
	return &FormatError{Kind: ErrCorrupt, Page: page, Offset: offset, Problem: fmt.Sprintf(format, args...)}
}

func corruptPage(page int, format string, args ...any) error {
	return corruptAt(page, -1, format, args...)
}
