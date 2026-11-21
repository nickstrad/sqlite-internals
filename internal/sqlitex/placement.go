package sqlitex

import "strings"

// SettingHome identifies whether an SQLite PRAGMA setting persists in the
// database file header or resets on every new connection.
type SettingHome string

const (
	// SettingInFile marks settings stored in the 100-byte database header
	// that survive closing and copying the file.
	SettingInFile SettingHome = "file"
	// SettingPerConnection marks settings held only in connection memory
	// that a platform must reapply on every open.
	SettingPerConnection SettingHome = "connection"
)

// SettingPlacement records where a course-tracked SQLite PRAGMA lives and,
// for file-stored settings, which header byte offsets store it.
type SettingPlacement struct {
	Name        string      `json:"name"`
	Pragma      string      `json:"pragma"`
	Home        SettingHome `json:"home"`
	HeaderBytes string      `json:"header_bytes,omitempty"`
}

// SettingPlacements returns the canonical table of every PRAGMA setting used
// across the course, distinguishing header-stored settings (including the
// deprecated default_cache_size at header bytes 48-51, read by sqlite3InitOne
// at src/prepare.c:323-333) from per-connection settings.
func SettingPlacements() []SettingPlacement {
	return []SettingPlacement{
		{Name: "page_size", Pragma: "page_size", Home: SettingInFile, HeaderBytes: "16-17"},
		{Name: "journal_mode=wal", Pragma: "journal_mode", Home: SettingInFile, HeaderBytes: "18-19"},
		{Name: "auto_vacuum", Pragma: "auto_vacuum", Home: SettingInFile, HeaderBytes: "52-55,64-67"},
		{Name: "encoding", Pragma: "encoding", Home: SettingInFile, HeaderBytes: "56-59"},
		{Name: "user_version", Pragma: "user_version", Home: SettingInFile, HeaderBytes: "60-63"},
		{Name: "application_id", Pragma: "application_id", Home: SettingInFile, HeaderBytes: "68-71"},
		{Name: "default_cache_size", Pragma: "default_cache_size", Home: SettingInFile, HeaderBytes: "48-51"},
		{Name: "journal_mode=" + strings.Join(RollbackJournalModes(), "|"), Pragma: "journal_mode", Home: SettingPerConnection},
		{Name: "synchronous", Pragma: "synchronous", Home: SettingPerConnection},
		{Name: "foreign_keys", Pragma: "foreign_keys", Home: SettingPerConnection},
		{Name: "cache_size", Pragma: "cache_size", Home: SettingPerConnection},
		{Name: "busy_timeout", Pragma: "busy_timeout", Home: SettingPerConnection},
		{Name: "wal_autocheckpoint", Pragma: "wal_autocheckpoint", Home: SettingPerConnection},
		{Name: "secure_delete", Pragma: "secure_delete", Home: SettingPerConnection},
	}
}

// RollbackJournalModes returns the five non-WAL journal modes, none of which
// are stored in header bytes 18-19 (all leave format version 1 and revert to
// delete on reopen).
func RollbackJournalModes() []string {
	return []string{JournalDelete, JournalTruncate, JournalPersist, JournalMemory, JournalOff}
}

// SettingPlacementFor looks up a setting placement entry by its Name field.
func SettingPlacementFor(name string) (SettingPlacement, bool) {
	for _, p := range SettingPlacements() {
		if p.Name == name {
			return p, true
		}
	}
	return SettingPlacement{}, false
}
