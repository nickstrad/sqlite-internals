package sqlitex

import (
	"path/filepath"
	"testing"
)

func TestResultCodeName(t *testing.T) {
	cases := map[int]string{CodeOK: "SQLITE_OK", CodeBusy: "SQLITE_BUSY", CodeBusySnapshot: "SQLITE_BUSY_SNAPSHOT", CodeBusyTimeout: "SQLITE_BUSY_TIMEOUT", CodeProtocol: "SQLITE_PROTOCOL", CodeIOErrDeleteNoEnt: "SQLITE_IOERR_DELETE_NOENT", CodeIOErr | 30<<8: "SQLITE_IOERR+extended(30)", 300: "SQLITE_code_300"}
	for code, want := range cases {
		if got := ResultCodeName(code); got != want {
			t.Errorf("ResultCodeName(%d) = %q, want %q", code, got, want)
		}
	}
	if CodeBusySnapshot != 517 || CodeBusyRecovery != 261 || CodeIOErrDeleteNoEnt != 5898 || CodeIOErrWrite != 778 {
		t.Fatal("extended code values differ from sqlite.h.in")
	}
}

func TestDisplayPath(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Skip(err)
	}
	if got := DisplayPath(filepath.Join(root, "internal", "sqlitex")); got != "internal/sqlitex" {
		t.Errorf("inside the repo: %q", got)
	}
	outside := filepath.Join(t.TempDir(), "scratch.db")
	if got := DisplayPath(outside); got != "scratch.db" {
		t.Errorf("outside the repo: %q", got)
	}
}
