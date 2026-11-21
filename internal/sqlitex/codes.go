package sqlitex

import "fmt"

// SQLite primary and extended result codes referenced by the locking and
// concurrency labs (src/sqlite.h.in:449-546 at version-3.53.4).
// CodeBusy (5) and CodeLocked (6) are declared in raw.go.
const (
	// CodeOK reports success (SQLITE_OK).
	CodeOK = 0
	// CodeError reports a generic SQL or runtime error (SQLITE_ERROR).
	CodeError = 1
	// CodeReadOnly reports a write attempt on a read-only database (SQLITE_READONLY).
	CodeReadOnly = 8
	// CodeIOErr reports an OS-level I/O error (SQLITE_IOERR).
	CodeIOErr = 10
	// CodeCorrupt reports a malformed database file (SQLITE_CORRUPT).
	CodeCorrupt = 11
	// CodeCantOpen reports that SQLite could not open the database file (SQLITE_CANTOPEN).
	CodeCantOpen = 14
	// CodeProtocol reports a locking-protocol failure, such as walTryBeginRead
	// exceeding WAL_RETRY_PROTOCOL_LIMIT (100) retries (src/wal.c:2963, 3053-3056).
	CodeProtocol = 15
	// CodeNotADB reports a file that does not begin with a valid SQLite header (SQLITE_NOTADB).
	CodeNotADB = 26
	// CodeLockedSharedCache reports table-lock contention inside shared-cache mode (262).
	CodeLockedSharedCache = CodeLocked | 1<<8
	// CodeLockedVTab reports a virtual-table lock conflict (518).
	CodeLockedVTab = CodeLocked | 2<<8
	// CodeBusyRecovery reports a WAL connection blocked by another process's recovery (261).
	CodeBusyRecovery = CodeBusy | 1<<8
	// CodeBusySnapshot reports a deferred read transaction trying to upgrade to a
	// write after another writer committed a newer snapshot (517, src/wal.c:3734-3737).
	CodeBusySnapshot = CodeBusy | 2<<8
	// CodeBusyTimeout reports a blocking lock timeout when SETLK_TIMEOUT is enabled (773).
	CodeBusyTimeout = CodeBusy | 3<<8
	// CodeIOErrShortRead reports a short read from xRead (522).
	CodeIOErrShortRead = CodeIOErr | 2<<8
	// CodeIOErrWrite reports a failed write from xWrite (778).
	CodeIOErrWrite = CodeIOErr | 3<<8
	// CodeIOErrLock reports an unexpected OS error while acquiring a file lock (3850).
	CodeIOErrLock = CodeIOErr | 15<<8
	// CodeIOErrShmLock reports an unexpected OS error on a -shm byte-range lock (5130).
	CodeIOErrShmLock = CodeIOErr | 20<<8
	// CodeIOErrDeleteNoEnt reports unixDelete seeing ENOENT when unlinking a file,
	// such as a rollback journal already deleted by another process (5898, src/os_unix.c:6842-6847).
	CodeIOErrDeleteNoEnt = CodeIOErr | 23<<8
)

// resultCodeNames maps known primary and extended codes to their SQLite C
// macro names because modernc.org/sqlite's ErrorCodeString omits several
// extended codes such as SQLITE_BUSY_SNAPSHOT (517).
var resultCodeNames = map[int]string{
	CodeOK:                "SQLITE_OK",
	CodeError:             "SQLITE_ERROR",
	CodeBusy:              "SQLITE_BUSY",
	CodeLocked:            "SQLITE_LOCKED",
	CodeReadOnly:          "SQLITE_READONLY",
	CodeIOErr:             "SQLITE_IOERR",
	CodeCorrupt:           "SQLITE_CORRUPT",
	CodeCantOpen:          "SQLITE_CANTOPEN",
	CodeProtocol:          "SQLITE_PROTOCOL",
	CodeNotADB:            "SQLITE_NOTADB",
	CodeLockedSharedCache: "SQLITE_LOCKED_SHAREDCACHE",
	CodeLockedVTab:        "SQLITE_LOCKED_VTAB",
	CodeBusyRecovery:      "SQLITE_BUSY_RECOVERY",
	CodeBusySnapshot:      "SQLITE_BUSY_SNAPSHOT",
	CodeBusyTimeout:       "SQLITE_BUSY_TIMEOUT",
	CodeIOErrShortRead:    "SQLITE_IOERR_SHORT_READ",
	CodeIOErrWrite:        "SQLITE_IOERR_WRITE",
	CodeIOErrLock:         "SQLITE_IOERR_LOCK",
	CodeIOErrShmLock:      "SQLITE_IOERR_SHMLOCK",
	CodeIOErrDeleteNoEnt:  "SQLITE_IOERR_DELETE_NOENT",
}

// ResultCodeName returns the SQLite macro name for a primary or extended result
// code. Unknown extended codes of a known primary code render as
// "<PRIMARY>+extended(<highByte>)"; completely unknown codes render as
// "SQLITE_code_<N>".
func ResultCodeName(code int) string {
	if name, ok := resultCodeNames[code]; ok {
		return name
	}
	// Extended result codes pack the primary code in the low 8 bits and the
	// subtype in bits 8 and above (src/sqlite.h.in:449-546).
	if name, ok := resultCodeNames[code&0xff]; ok && code > 0xff {
		return fmt.Sprintf("%s+extended(%d)", name, code>>8)
	}
	return fmt.Sprintf("SQLITE_code_%d", code)
}
