package connmgr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const (
	// SQLite places its Unix POSIX advisory locks starting at PENDING_BYTE
	// (0x40000000): PENDING (1 byte), RESERVED (1 byte), and SHARED (510 bytes)
	// in src/os.h:160-166 and src/os_unix.c:1866-2095.
	sqliteLockRegionStart  = 0x40000000
	sqliteLockRegionLength = 2 + 510
)

// Status describes whether a database file is at rest (a complete, unheld
// single file safe to copy) along with its SHA-256 and byte size when AtRest
// is true, or the reasons it is not at rest in Problems.
type Status struct {
	Path     string
	AtRest   bool
	Problems []string
	SHA256   string
	Size     int64
}

// String formats Status as either an at-rest summary (size and sha256) or a
// semicolon-separated list of problems.
func (s Status) String() string {
	if s.AtRest {
		return fmt.Sprintf("at rest, %d bytes, sha256 %s", s.Size, s.SHA256)
	}
	return "not at rest: " + strings.Join(s.Problems, "; ")
}

// CheckAtRest verifies that path is a single complete database file safe to
// copy: the file exists, no non-empty -wal remains, no hot rollback -journal
// exists, no -shm exists, and no other process holds a POSIX lock on SQLite's
// lock byte range. When all checks pass, it hashes the file on the same
// descriptor and returns AtRest=true with SHA256 and Size populated.
//
// Hazard: CheckAtRest opens and closes path. On Unix, closing any descriptor
// to an inode releases every POSIX lock the current process holds on that
// inode, so callers must invoke CheckAtRest only when this process has no open
// SQLite connection to path.
func CheckAtRest(path string) (Status, error) {
	status := Status{Path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		status.Problems = append(status.Problems, "database file missing")
		return status, nil
	} else if err != nil {
		return status, err
	}
	// A zero-byte -wal (e.g. left by wal_checkpoint(TRUNCATE)) holds no frames;
	// any positive size may hold committed frames not yet checkpointed.
	wal, err := os.Stat(path + "-wal")
	switch {
	case err == nil && wal.Size() > 0:
		status.Problems = append(status.Problems, fmt.Sprintf("-wal holds %d bytes: the database file alone may lack committed transactions", wal.Size()))
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return status, err
	}
	hot, journalSize, err := hotJournal(path + "-journal")
	if err != nil {
		return status, err
	}
	if hot {
		status.Problems = append(status.Problems, fmt.Sprintf("hot -journal (%d bytes): the next open rolls back an interrupted transaction", journalSize))
	}
	// In normal WAL mode, sqlite3WalClose (src/wal.c:2507-2571) unlinks -shm
	// via walIndexClose (src/wal.c:1619-1630) when the last connection closes
	// cleanly unless PERSIST_WAL is set.
	shm, err := os.Stat(path + "-shm")
	switch {
	case err == nil:
		status.Problems = append(status.Problems, fmt.Sprintf("-shm exists (%d bytes): a connection is open or did not close cleanly", shm.Size()))
	case !errors.Is(err, os.ErrNotExist):
		return status, err
	}
	if len(status.Problems) > 0 {
		return status, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return status, err
	}
	defer file.Close()
	// Side-file checks alone miss an exclusive-mode WAL connection (which keeps
	// its wal-index in heap memory without creating -shm) and an active
	// rollback-mode reader in another process; both hold locks on the db file.
	locked, err := lockedByAnotherProcess(file)
	if err != nil {
		return status, err
	}
	if locked {
		status.Problems = append(status.Problems, "another process holds a SQLite lock on the database file: it has the database open")
		return status, nil
	}
	sum, size, err := fileSHA256(file)
	if err != nil {
		return status, err
	}
	status.AtRest, status.SHA256, status.Size = true, sum, size
	return status, nil
}

// lockedByAnotherProcess uses fcntl(F_GETLK) to test whether a write lock over
// SQLite's PENDING/RESERVED/SHARED byte range would conflict with a lock held
// by another process. POSIX F_GETLK never reports locks held by the calling
// process itself.
func lockedByAnotherProcess(file *os.File) (bool, error) {
	probe := syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: 0,
		Start:  sqliteLockRegionStart,
		Len:    sqliteLockRegionLength,
	}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &probe); err != nil {
		return false, fmt.Errorf("connmgr: probe locks on %s: %w", file.Name(), err)
	}
	return probe.Type != syscall.F_UNLCK, nil
}

func fileSHA256(file *os.File) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// hotJournal mirrors SQLite's hot-journal header check (hasHotJournal,
// src/pager.c:5180-5271): a journal file that exists, has nonzero length, and
// starts with a nonzero byte (not zeroed by PERSIST mode) represents an
// interrupted write transaction.
func hotJournal(path string) (bool, int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, 0, err
	}
	if info.Size() == 0 {
		return false, 0, nil
	}
	first := make([]byte, 1)
	if _, err := file.ReadAt(first, 0); err != nil {
		return false, info.Size(), err
	}
	return first[0] != 0, info.Size(), nil
}
