package sqlitex

import "fmt"

const (
	CodeOK                = 0
	CodeError             = 1
	CodeReadOnly          = 8
	CodeIOErr             = 10
	CodeCorrupt           = 11
	CodeCantOpen          = 14
	CodeProtocol          = 15
	CodeNotADB            = 26
	CodeLockedSharedCache = CodeLocked | 1<<8
	CodeLockedVTab        = CodeLocked | 2<<8
	CodeBusyRecovery      = CodeBusy | 1<<8
	CodeBusySnapshot      = CodeBusy | 2<<8
	CodeBusyTimeout       = CodeBusy | 3<<8
	CodeIOErrShortRead    = CodeIOErr | 2<<8
	CodeIOErrWrite        = CodeIOErr | 3<<8
	CodeIOErrLock         = CodeIOErr | 15<<8
	CodeIOErrShmLock      = CodeIOErr | 20<<8
	CodeIOErrDeleteNoEnt  = CodeIOErr | 23<<8
)

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

func ResultCodeName(code int) string {
	if name, ok := resultCodeNames[code]; ok {
		return name
	}
	if name, ok := resultCodeNames[code&0xff]; ok && code > 0xff {
		return fmt.Sprintf("%s+extended(%d)", name, code>>8)
	}
	return fmt.Sprintf("SQLITE_code_%d", code)
}
