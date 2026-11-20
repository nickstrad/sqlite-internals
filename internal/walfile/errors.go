package walfile

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by WAL parsing, replay, and wal-index decoding.
var (
	// ErrWALIgnored marks a WAL header that SQLite silently treats as having
	// zero valid frames during recovery (src/wal.c:1447-1467, walIndexRecover).
	ErrWALIgnored = errors.New("SQLite ignores this WAL file")
	// ErrUnsupportedVersion marks a WAL header whose format version is not
	// 3007000; SQLite returns SQLITE_CANTOPEN (src/wal.c:1472-1474).
	ErrUnsupportedVersion = errors.New("unsupported WAL format version")
	// ErrBaseNotDatabase reports a non-empty replay base missing the 100-byte
	// "SQLite format 3\x00" database header.
	ErrBaseNotDatabase = errors.New("base is not an SQLite database file")
	// ErrBaseSize reports a base database whose byte length is not a whole
	// multiple of the WAL page size.
	ErrBaseSize = errors.New("base size is not a whole number of pages")
	// ErrPageSizeMismatch reports that the base database header and WAL header
	// disagree on page size (src/wal.c:4392, SQLITE_CORRUPT).
	ErrPageSizeMismatch = errors.New("base and WAL page sizes differ")
	// ErrCommitSizeTooLarge reports a commit size larger than base + 65536 +
	// frames*pageSize, matching SQLite's corruption guard in walCheckpoint
	// (src/wal.c:2288-2293).
	ErrCommitSizeTooLarge = errors.New("commit size is larger than the base plus every frame could fill")
	// ErrOutputExists reports that the replay target path or a sibling -wal,
	// -journal, or -shm file already exists and could corrupt a later open.
	ErrOutputExists = errors.New("output file or one of its side files already exists")
	// ErrShortIndex reports a -shm file smaller than the 136-byte wal-index
	// header region (src/wal.c:474, WALINDEX_HDR_SIZE).
	ErrShortIndex = errors.New("wal-index is shorter than its header")
	// ErrIndexVersion reports a non-zero -shm header whose first word is not
	// 3007000 in either little-endian or big-endian byte order.
	ErrIndexVersion = errors.New("wal-index header version is not 3007000 in either byte order")
	// ErrFrameNotIndexed reports a frame number outside the mapped -shm blocks
	// or a lookup against an uninitialized wal-index.
	ErrFrameNotIndexed = errors.New("frame is outside the wal-index")
)

// HeaderError pairs a sentinel error (ErrWALIgnored or ErrUnsupportedVersion)
// with the specific header field that failed validation.
type HeaderError struct {
	Kind   error
	Reason string
}

// Error formats the sentinel error followed by the specific header reason.
func (e *HeaderError) Error() string {
	return fmt.Sprintf("%s: %s", e.Kind, e.Reason)
}

// Unwrap returns Kind so errors.Is matches ErrWALIgnored or ErrUnsupportedVersion.
func (e *HeaderError) Unwrap() error { return e.Kind }

func ignored(format string, args ...any) error {
	return &HeaderError{Kind: ErrWALIgnored, Reason: fmt.Sprintf(format, args...)}
}
