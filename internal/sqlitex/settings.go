package sqlitex

import (
	"fmt"
	"strings"
	"time"
)

const CourseApplicationID int32 = 0x53514958

const (
	JournalWAL      = "wal"
	JournalDelete   = "delete"
	JournalTruncate = "truncate"
	JournalPersist  = "persist"
	JournalMemory   = "memory"
	JournalOff      = "off"
)

const (
	SyncOff    = "OFF"
	SyncNormal = "NORMAL"
	SyncFull   = "FULL"
	SyncExtra  = "EXTRA"
)

const (
	AutoVacuumNone        = "NONE"
	AutoVacuumFull        = "FULL"
	AutoVacuumIncremental = "INCREMENTAL"
)

type FileSettings struct {
	PageSize      int
	AutoVacuum    string
	Encoding      string
	ApplicationID int32
	UserVersion   int32
	JournalMode   string
}

type ConnSettings struct {
	Synchronous       string
	ForeignKeys       bool
	SecureDelete      bool
	CacheSize         int
	WALAutocheckpoint int
	BusyTimeout       time.Duration
}

type Options struct {
	File             FileSettings
	Conn             ConnSettings
	SingleConnection bool
	ImmediateTx      bool
	ReadOnly         bool
}

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

func DefaultOptions() Options {
	return Options{
		File:        DefaultFileSettings(),
		Conn:        DefaultConnSettings(),
		ImmediateTx: true,
	}
}

var synchronousLevels = map[string]int64{SyncOff: 0, SyncNormal: 1, SyncFull: 2, SyncExtra: 3}

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

func (c ConnSettings) Validate() error {
	if _, err := synchronousLevel(c.Synchronous); err != nil {
		return err
	}
	if c.BusyTimeout < 0 {
		return fmt.Errorf("busy timeout %v is negative", c.BusyTimeout)
	}
	if c.WALAutocheckpoint < 0 {
		return fmt.Errorf("wal_autocheckpoint %d is negative", c.WALAutocheckpoint)
	}
	return nil
}

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

func (f FileSettings) creationPragmas() []string {
	return []string{
		fmt.Sprintf("PRAGMA page_size=%d", f.PageSize),
		"PRAGMA auto_vacuum=" + strings.ToUpper(f.AutoVacuum),
		"PRAGMA encoding='" + strings.ToUpper(f.Encoding) + "'",
		fmt.Sprintf("PRAGMA application_id=%d", f.ApplicationID),
		fmt.Sprintf("PRAGMA user_version=%d", f.UserVersion),
	}
}

func (f FileSettings) journalPragma() string {
	return "PRAGMA journal_mode=" + strings.ToLower(f.JournalMode)
}

func (c ConnSettings) String() string {
	return fmt.Sprintf("synchronous=%s foreign_keys=%s secure_delete=%s cache_size=%d wal_autocheckpoint=%d busy_timeout=%dms",
		strings.ToUpper(c.Synchronous), boolPragma(c.ForeignKeys), boolPragma(c.SecureDelete),
		c.CacheSize, c.WALAutocheckpoint, c.BusyTimeout.Milliseconds())
}

func (f FileSettings) String() string {
	return fmt.Sprintf("page_size=%d auto_vacuum=%s encoding=%s application_id=%d user_version=%d journal_mode=%s",
		f.PageSize, strings.ToUpper(f.AutoVacuum), strings.ToUpper(f.Encoding), f.ApplicationID, f.UserVersion,
		strings.ToLower(f.JournalMode))
}
