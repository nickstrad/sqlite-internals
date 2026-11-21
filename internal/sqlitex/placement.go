package sqlitex

import "strings"

type SettingHome string

const (
	SettingInFile        SettingHome = "file"
	SettingPerConnection SettingHome = "connection"
)

type SettingPlacement struct {
	Name        string      `json:"name"`
	Pragma      string      `json:"pragma"`
	Home        SettingHome `json:"home"`
	HeaderBytes string      `json:"header_bytes,omitempty"`
}

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

func RollbackJournalModes() []string {
	return []string{JournalDelete, JournalTruncate, JournalPersist, JournalMemory, JournalOff}
}

func SettingPlacementFor(name string) (SettingPlacement, bool) {
	for _, p := range SettingPlacements() {
		if p.Name == name {
			return p, true
		}
	}
	return SettingPlacement{}, false
}
