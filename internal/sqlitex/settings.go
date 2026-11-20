// Package sqlitex opens file-backed and in-memory SQLite databases with
// explicit, verified settings, checks the pinned SQLite revision and driver
// modules, wraps modernc.org/sqlite driver calls reached through sql.Conn.Raw,
// manages scratch directories, and locates the pinned sqlite3 CLI and companion tools.
package sqlitex

import (
	"fmt"
	"strings"
	"time"
)

// CourseApplicationID is the 4-byte ASCII tag "SQIX" (1397836120) written to
// header bytes 68-71 (PRAGMA application_id) on every database the course creates.
const CourseApplicationID int32 = 0x53514958

// Journal mode names accepted by PRAGMA journal_mode. Only JournalWAL persists
// in the database header (bytes 18-19); all rollback modes apply per connection.
const (
	JournalWAL      = "wal"
	JournalDelete   = "delete"
	JournalTruncate = "truncate"
	JournalPersist  = "persist"
	JournalMemory   = "memory"
	JournalOff      = "off"
)

// Synchronous durability levels accepted by PRAGMA synchronous. They control
// how often SQLite asks the operating system to flush writes with fsync or fdatasync.
const (
	SyncOff    = "OFF"
	SyncNormal = "NORMAL"
	SyncFull   = "FULL"
	SyncExtra  = "EXTRA"
)

// Auto-vacuum modes accepted by PRAGMA auto_vacuum. The mode is stored in the
// database header and can only be chosen before the first page is written.
const (
	AutoVacuumNone        = "NONE"
	AutoVacuumFull        = "FULL"
	AutoVacuumIncremental = "INCREMENTAL"
)

// FileSettings holds persistent settings stored inside the SQLite database file.
// Setting every field explicitly keeps database files reproducible across runs.
type FileSettings struct {
	PageSize      int
	AutoVacuum    string
	Encoding      string
	ApplicationID int32
	UserVersion   int32
	JournalMode   string
}

// ConnSettings holds per-connection settings that SQLite resets to defaults on
// every open. The package reapplies and verifies them on each physical connection.
type ConnSettings struct {
	Synchronous       string
	ForeignKeys       bool
	SecureDelete      bool
	CacheSize         int
	WALAutocheckpoint int
	BusyTimeout       time.Duration
}

// Options combines file settings, per-connection settings, and pool behavior
// passed to Open.
type Options struct {
	File             FileSettings
	Conn             ConnSettings
	SingleConnection bool
	ImmediateTx      bool
	ReadOnly         bool
}

// DefaultFileSettings returns the course's baseline persistent file settings:
// 4096-byte pages, auto_vacuum NONE, UTF-8 encoding, CourseApplicationID,
// user_version 0, and WAL (write-ahead log) journal mode.
func DefaultFileSettings() FileSettings {
	return FileSettings{
		PageSize:      4096,
		AutoVacuum:    AutoVacuumNone,
		Encoding:      "UTF-8",
		ApplicationID: CourseApplicationID,
		UserVersion:   0,
		JournalMode:   JournalWAL,
	}
}

// DefaultConnSettings returns the course's baseline per-connection settings:
// synchronous FULL (SQLite's upstream default in WAL mode), foreign keys on,
// secure_delete off, a 2000 KiB page cache (-2000), WAL auto-checkpoint at
// 1000 frames, and a 5-second busy timeout.
func DefaultConnSettings() ConnSettings {
	return ConnSettings{
		Synchronous:       SyncFull,
		ForeignKeys:       true,
		SecureDelete:      false,
		CacheSize:         -2000,
		WALAutocheckpoint: 1000,
		BusyTimeout:       5 * time.Second,
	}
}

// DefaultOptions returns DefaultFileSettings, DefaultConnSettings, and
// ImmediateTx enabled so BEGIN acquires the write lock up front.
func DefaultOptions() Options {
	return Options{
		File:        DefaultFileSettings(),
		Conn:        DefaultConnSettings(),
		ImmediateTx: true,
	}
}

// PRAGMA synchronous reports integer levels 0..3 when queried.
var synchronousLevels = map[string]int64{SyncOff: 0, SyncNormal: 1, SyncFull: 2, SyncExtra: 3}

// PRAGMA auto_vacuum reports integer levels 0..2 when queried.
var autoVacuumLevels = map[string]int64{AutoVacuumNone: 0, AutoVacuumFull: 1, AutoVacuumIncremental: 2}

func synchronousLevel(name string) (int64, error) {
	level, ok := synchronousLevels[strings.ToUpper(name)]
	if !ok {
		return 0, fmt.Errorf("unknown synchronous level %q", name)
	}
	return level, nil
}

func synchronousName(level int64) string {
	for name, value := range synchronousLevels {
		if value == level {
			return name
		}
	}
	return fmt.Sprintf("UNKNOWN(%d)", level)
}

func autoVacuumLevel(name string) (int64, error) {
	level, ok := autoVacuumLevels[strings.ToUpper(name)]
	if !ok {
		return 0, fmt.Errorf("unknown auto_vacuum mode %q", name)
	}
	return level, nil
}

func autoVacuumName(level int64) string {
	for name, value := range autoVacuumLevels {
		if value == level {
			return name
		}
	}
	return fmt.Sprintf("UNKNOWN(%d)", level)
}

func boolPragma(value bool) string {
	if value {
		return "ON"
	}
	return "OFF"
}

// Validate checks that every per-connection setting has a value SQLite accepts,
// including requiring BusyTimeout to be a non-negative whole number of milliseconds.
func (c ConnSettings) Validate() error {
	if _, err := synchronousLevel(c.Synchronous); err != nil {
		return err
	}
	if c.BusyTimeout < 0 {
		return fmt.Errorf("busy timeout %v is negative", c.BusyTimeout)
	}
	if c.BusyTimeout%time.Millisecond != 0 {
		return fmt.Errorf("busy timeout %v is not a whole number of milliseconds (PRAGMA busy_timeout takes milliseconds)", c.BusyTimeout)
	}
	if c.WALAutocheckpoint < 0 {
		return fmt.Errorf("wal_autocheckpoint %d is negative", c.WALAutocheckpoint)
	}
	return nil
}

// Validate checks that every persistent file setting is valid in SQLite:
// page_size must be a power of two from 512 to 65536, and auto_vacuum,
// encoding, and journal_mode must name supported modes.
func (f FileSettings) Validate() error {
	if f.PageSize < 512 || f.PageSize > 65536 || f.PageSize&(f.PageSize-1) != 0 {
		return fmt.Errorf("page size %d is not a power of two between 512 and 65536", f.PageSize)
	}
	if _, err := autoVacuumLevel(f.AutoVacuum); err != nil {
		return err
	}
	switch strings.ToUpper(f.Encoding) {
	case "UTF-8", "UTF-16LE", "UTF-16BE":
	default:
		return fmt.Errorf("unknown encoding %q", f.Encoding)
	}
	switch strings.ToLower(f.JournalMode) {
	case JournalWAL, JournalDelete, JournalTruncate, JournalPersist, JournalMemory, JournalOff:
	default:
		return fmt.Errorf("unknown journal mode %q", f.JournalMode)
	}
	return nil
}

// pragmas returns the per-connection PRAGMA statements with busy_timeout first
// so configureConnection can run it before any statement that may contend on a lock.
func (c ConnSettings) pragmas() []string {
	return []string{
		fmt.Sprintf("PRAGMA busy_timeout=%d", c.BusyTimeout.Milliseconds()),
		"PRAGMA synchronous=" + strings.ToUpper(c.Synchronous),
		"PRAGMA foreign_keys=" + boolPragma(c.ForeignKeys),
		"PRAGMA secure_delete=" + boolPragma(c.SecureDelete),
		fmt.Sprintf("PRAGMA cache_size=%d", c.CacheSize),
		fmt.Sprintf("PRAGMA wal_autocheckpoint=%d", c.WALAutocheckpoint),
	}
}

// layoutPragmas returns the header settings that must run before page 1 is written.
func (f FileSettings) layoutPragmas() []string {
	return []string{
		fmt.Sprintf("PRAGMA page_size=%d", f.PageSize),
		"PRAGMA auto_vacuum=" + strings.ToUpper(f.AutoVacuum),
		"PRAGMA encoding='" + strings.ToUpper(f.Encoding) + "'",
	}
}

// identityPragmas returns the header metadata stamps applied after journal_mode is established.
func (f FileSettings) identityPragmas() []string {
	return []string{
		fmt.Sprintf("PRAGMA application_id=%d", f.ApplicationID),
		fmt.Sprintf("PRAGMA user_version=%d", f.UserVersion),
	}
}

func (f FileSettings) journalPragma() string {
	return "PRAGMA journal_mode=" + strings.ToLower(f.JournalMode)
}

// String formats all per-connection settings on one line for experiment output.
func (c ConnSettings) String() string {
	return fmt.Sprintf("synchronous=%s foreign_keys=%s secure_delete=%s cache_size=%d wal_autocheckpoint=%d busy_timeout=%dms",
		strings.ToUpper(c.Synchronous), boolPragma(c.ForeignKeys), boolPragma(c.SecureDelete),
		c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

// String formats all persistent file settings on one line for experiment output.
func (f FileSettings) String() string {
	return fmt.Sprintf("page_size=%d auto_vacuum=%s encoding=%s application_id=%d user_version=%d journal_mode=%s",
		f.PageSize, strings.ToUpper(f.AutoVacuum), strings.ToUpper(f.Encoding), f.ApplicationID, f.UserVersion,
		strings.ToLower(f.JournalMode))
}
