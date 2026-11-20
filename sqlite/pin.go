package sqlite

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed PIN
var pinFile string

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

func (p Pin) SourceDirName() string {
	return "sqlite-" + p.Release
}

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
	if !strings.HasSuffix(pin.SourceID, " "+pin.FossilID) {
		return Pin{}, fmt.Errorf("PIN: source_id %q does not end with fossil_id %q", pin.SourceID, pin.FossilID)
	}
	return pin, nil
}

func Current() Pin {
	pin, err := ParsePin(pinFile)
	if err != nil {
		panic(err)
	}
	return pin
}
