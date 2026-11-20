package govfs

import (
	"fmt"
	"sort"
	"strings"
)

type FileKind string

const (
	KindMainDB       FileKind = "main-db"
	KindMainJournal  FileKind = "main-journal"
	KindWAL          FileKind = "wal"
	KindTempDB       FileKind = "temp-db"
	KindTempJournal  FileKind = "temp-journal"
	KindTransientDB  FileKind = "transient-db"
	KindSubjournal   FileKind = "subjournal"
	KindSuperJournal FileKind = "super-journal"
	KindOther        FileKind = "other"
	KindNone         FileKind = ""
)

const (
	OpenReadOnly      = 0x00000001
	OpenReadWrite     = 0x00000002
	OpenCreate        = 0x00000004
	OpenDeleteOnClose = 0x00000008
	OpenExclusive     = 0x00000010
	OpenMainDB        = 0x00000100
	OpenTempDB        = 0x00000200
	OpenTransientDB   = 0x00000400
	OpenMainJournal   = 0x00000800
	OpenTempJournal   = 0x00001000
	OpenSubjournal    = 0x00002000
	OpenSuperJournal  = 0x00004000
	OpenWAL           = 0x00080000
)

const (
	SyncNormal   = 0x00002
	SyncFull     = 0x00003
	SyncDataOnly = 0x00010
)

const (
	CodeOK    int32 = 0
	CodeIOErr int32 = 10
)

func KindFromOpenFlags(flags int32) FileKind {
	switch {
	case flags&OpenMainDB != 0:
		return KindMainDB
	case flags&OpenMainJournal != 0:
		return KindMainJournal
	case flags&OpenWAL != 0:
		return KindWAL
	case flags&OpenTempDB != 0:
		return KindTempDB
	case flags&OpenTempJournal != 0:
		return KindTempJournal
	case flags&OpenTransientDB != 0:
		return KindTransientDB
	case flags&OpenSubjournal != 0:
		return KindSubjournal
	case flags&OpenSuperJournal != 0:
		return KindSuperJournal
	}
	return KindOther
}

type Call struct {
	Seq      int
	Method   string
	File     string
	Kind     FileKind
	Amount   int64
	Offset   int64
	Flags    int64
	Out      int64
	Result   int32
	Injected bool
}

func (c Call) String() string {
	var args string
	switch c.Method {
	case "xOpen":
		args = fmt.Sprintf("kind=%s flags=0x%x", c.Kind, c.Flags)
	case "xDelete":
		args = fmt.Sprintf("syncDir=%d", c.Flags)
	case "xAccess":
		args = fmt.Sprintf("flags=%d", c.Flags)
	case "xRead", "xWrite":
		args = fmt.Sprintf("n=%d ofst=%d", c.Amount, c.Offset)
	case "xTruncate":
		args = fmt.Sprintf("size=%d", c.Amount)
	case "xSync":
		args = "flags=" + SyncFlagsString(c.Flags)
	case "xLock", "xUnlock":
		args = "level=" + LockLevelString(c.Flags)
	case "xFileControl":
		args = "op=" + FileControlName(c.Flags)
	case "xShmMap":
		args = fmt.Sprintf("region=%d size=%d extend=%d", c.Offset, c.Amount, c.Flags)
	case "xShmLock":
		args = fmt.Sprintf("ofst=%d n=%d flags=%s", c.Offset, c.Amount, ShmLockFlagsString(c.Flags))
	case "xShmUnmap":
		args = fmt.Sprintf("delete=%d", c.Flags)
	case "xFetch":
		args = fmt.Sprintf("ofst=%d n=%d", c.Offset, c.Amount)
	case "xUnfetch":
		args = fmt.Sprintf("ofst=%d", c.Offset)
	case "xRandomness":
		args = fmt.Sprintf("n=%d", c.Amount)
	case "xSleep":
		args = fmt.Sprintf("us=%d", c.Amount)
	}
	var b strings.Builder
	b.WriteString(c.Method)
	if c.File != "" {
		b.WriteString(" " + c.File)
	}
	if args != "" {
		b.WriteString(" " + args)
	}
	switch c.Method {
	case "xShmBarrier":
	case "xDeviceCharacteristics":
		fmt.Fprintf(&b, " -> 0x%x", c.Result)
	default:
		fmt.Fprintf(&b, " -> %d", c.Result)
	}
	switch c.Method {
	case "xAccess", "xCheckReservedLock":
		fmt.Fprintf(&b, " out=%d", c.Out)
	case "xFileSize":
		fmt.Fprintf(&b, " size=%d", c.Out)
	}
	if c.Injected {
		b.WriteString(" (injected)")
	}
	return b.String()
}

func SyncFlagsString(flags int64) string {
	var parts []string
	switch flags & SyncFull {
	case SyncFull:
		parts = append(parts, "FULL")
	case SyncNormal:
		parts = append(parts, "NORMAL")
	}
	if flags&SyncDataOnly != 0 {
		parts = append(parts, "DATAONLY")
	}
	if rest := flags &^ (SyncFull | SyncDataOnly); rest != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", rest))
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

func VFSTraceSyncLabel(flags int64) string {
	var parts []string
	if flags&SyncFull != 0 {
		parts = append(parts, "FULL")
	} else if flags&SyncNormal != 0 {
		parts = append(parts, "NORMAL")
	}
	if flags&SyncDataOnly != 0 {
		parts = append(parts, "DATAONLY")
	}
	if flags&^(SyncFull|SyncDataOnly) != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", flags))
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

var lockLevels = []string{"NONE", "SHARED", "RESERVED", "PENDING", "EXCLUSIVE"}

func LockLevelString(level int64) string {
	if level >= 0 && int(level) < len(lockLevels) {
		return lockLevels[level]
	}
	return fmt.Sprintf("%d", level)
}

const (
	ShmUnlock    = 1
	ShmLock      = 2
	ShmShared    = 4
	ShmExclusive = 8
)

func ShmLockFlagsString(flags int64) string {
	var parts []string
	if flags&ShmUnlock != 0 {
		parts = append(parts, "UNLOCK")
	}
	if flags&ShmLock != 0 {
		parts = append(parts, "LOCK")
	}
	if flags&ShmShared != 0 {
		parts = append(parts, "SHARED")
	}
	if flags&ShmExclusive != 0 {
		parts = append(parts, "EXCLUSIVE")
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, "|")
}

type Counts map[string]int

func CountCalls(calls []Call) Counts {
	counts := Counts{}
	for _, c := range calls {
		counts[c.Method]++
	}
	return counts
}

func (c Counts) String() string {
	methods := make([]string, 0, len(c))
	for m := range c {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	parts := make([]string, len(methods))
	for i, m := range methods {
		parts[i] = fmt.Sprintf("%s=%d", m, c[m])
	}
	return strings.Join(parts, " ")
}

func Filter(calls []Call, keep func(Call) bool) []Call {
	var out []Call
	for _, c := range calls {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func Methods(names ...string) func(Call) bool {
	return func(c Call) bool {
		for _, n := range names {
			if c.Method == n {
				return true
			}
		}
		return false
	}
}

var fileControlNames = map[int64]string{
	1: "LOCKSTATE", 2: "GET_LOCKPROXYFILE", 3: "SET_LOCKPROXYFILE", 4: "LAST_ERRNO", 5: "SIZE_HINT",
	6: "CHUNK_SIZE", 7: "FILE_POINTER", 8: "SYNC_OMITTED", 9: "WIN32_AV_RETRY", 10: "PERSIST_WAL",
	11: "OVERWRITE", 12: "VFSNAME", 13: "POWERSAFE_OVERWRITE", 14: "PRAGMA", 15: "BUSYHANDLER",
	16: "TEMPFILENAME", 18: "MMAP_SIZE", 19: "TRACE", 20: "HAS_MOVED", 21: "SYNC",
	22: "COMMIT_PHASETWO", 23: "WIN32_SET_HANDLE", 24: "WAL_BLOCK", 25: "ZIPVFS", 26: "RBU",
	27: "VFS_POINTER", 28: "JOURNAL_POINTER", 29: "WIN32_GET_HANDLE", 30: "PDB",
	31: "BEGIN_ATOMIC_WRITE", 32: "COMMIT_ATOMIC_WRITE", 33: "ROLLBACK_ATOMIC_WRITE",
	34: "LOCK_TIMEOUT", 35: "DATA_VERSION", 36: "SIZE_LIMIT", 37: "CKPT_DONE", 38: "RESERVE_BYTES",
	39: "CKPT_START", 40: "EXTERNAL_READER", 41: "CKSM_FILE", 42: "RESET_CACHE", 43: "NULL_IO",
	44: "BLOCK_ON_CONNECT", 45: "FILESTAT",
}

func FileControlName(op int64) string {
	if name, ok := fileControlNames[op]; ok {
		return name
	}
	return fmt.Sprintf("%d", op)
}
