// Package sqlite embeds and parses the course's pinned SQLite revision from
// sqlite/PIN so every experiment and repository check verifies against the same
// release, commit, and Go driver version.
package sqlite

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed PIN
var pinFile string

// Pin records the exact SQLite revision and Go driver version pinned for the
// course. Every database header stores the writer's library version in bytes
// 96-99, so reproducible byte-level experiments require one fixed build.
type Pin struct {
	Release       string
	ReleaseDate   string
	Tag           string
	GitCommit     string
	FossilID      string
	SourceID      string
	DriverModule  string
	DriverVersion string
	LibcModule    string
	LibcVersion   string
	GitRepo       string
}

// SourceDirName returns the default directory name for the unpacked pinned
// SQLite source tree (for example, "sqlite-3.53.4").
func (p Pin) SourceDirName() string {
	return "sqlite-" + p.Release
}

// ParsePin parses key=value lines in the format of sqlite/PIN, skipping blank
// lines and '#' comments. It rejects malformed lines, duplicate keys, missing
// required fields, and a source_id that does not end with fossil_id.
func ParsePin(text string) (Pin, error) {
	fields := map[string]string{}
	for lineNumber, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return Pin{}, fmt.Errorf("PIN line %d: missing '=': %q", lineNumber+1, line)
		}
		key = strings.TrimSpace(key)
		if _, duplicate := fields[key]; duplicate {
			return Pin{}, fmt.Errorf("PIN line %d: duplicate key %q", lineNumber+1, key)
		}
		fields[key] = strings.TrimSpace(value)
	}
	pin := Pin{
		Release:       fields["release"],
		ReleaseDate:   fields["release_date"],
		Tag:           fields["tag"],
		GitCommit:     fields["git_commit"],
		FossilID:      fields["fossil_id"],
		SourceID:      fields["source_id"],
		DriverModule:  fields["driver_module"],
		DriverVersion: fields["driver_version"],
		LibcModule:    fields["libc_module"],
		LibcVersion:   fields["libc_version"],
		GitRepo:       fields["git_repo"],
	}
	required := map[string]string{
		"release": pin.Release, "tag": pin.Tag, "git_commit": pin.GitCommit,
		"fossil_id": pin.FossilID, "source_id": pin.SourceID,
		"driver_module": pin.DriverModule, "driver_version": pin.DriverVersion,
		"git_repo": pin.GitRepo,
	}
	for key, value := range required {
		if value == "" {
			return Pin{}, fmt.Errorf("PIN: required key %q is missing or empty", key)
		}
	}
	// SQLite's sqlite_source_id() returns "<UTC timestamp> <Fossil check-in hash>",
	// where the hash matches manifest.uuid in the pinned tree.
	if !strings.HasSuffix(pin.SourceID, " "+pin.FossilID) {
		return Pin{}, fmt.Errorf("PIN: source_id %q does not end with fossil_id %q", pin.SourceID, pin.FossilID)
	}
	return pin, nil
}

// Current returns the embedded course pin parsed from sqlite/PIN. It panics if
// the embedded file is malformed because a broken PIN is a build defect.
func Current() Pin {
	pin, err := ParsePin(pinFile)
	if err != nil {
		panic(err)
	}
	return pin
}
