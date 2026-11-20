package walfile

import (
	"errors"
	"fmt"
)

var (
	ErrWALIgnored         = errors.New("SQLite ignores this WAL file")
	ErrUnsupportedVersion = errors.New("unsupported WAL format version")
	ErrBaseNotDatabase    = errors.New("base is not an SQLite database file")
	ErrBaseSize           = errors.New("base size is not a whole number of pages")
	ErrPageSizeMismatch   = errors.New("base and WAL page sizes differ")
	ErrCommitSizeTooLarge = errors.New("commit size is larger than the base plus every frame could fill")
	ErrOutputExists       = errors.New("output file or one of its side files already exists")
	ErrShortIndex         = errors.New("wal-index is shorter than its header")
	ErrIndexVersion       = errors.New("wal-index header version is not 3007000 in either byte order")
	ErrFrameNotIndexed    = errors.New("frame is outside the wal-index")
)

type HeaderError struct {
	Kind   error
	Reason string
}

func (e *HeaderError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Reason)
}

func (e *HeaderError) Unwrap() error { return e.Kind }

func ignored(format string, args ...any) error {
	return &HeaderError{Kind: ErrWALIgnored, Reason: fmt.Sprintf(format, args...)}
}
