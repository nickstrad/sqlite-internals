package sqlitex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/nickstrad/sqlite-internals/sqlite"
)

// PinMismatchError reports that a Go driver or sqlite3 CLI binary reported a
// sqlite_source_id() different from sqlite/PIN.
type PinMismatchError struct {
	Got  string
	Want string
	What string
}

// Error formats the mismatch naming the component, observed ID, and pinned ID.
func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("%s reports sqlite_source_id %q, the pin is %q", e.What, e.Got, e.Want)
}

// SourceID queries SELECT sqlite_source_id() on q.
func SourceID(ctx context.Context, q Querier) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, "SELECT sqlite_source_id()").Scan(&id)
	return id, err
}

// CheckPin queries sqlite_source_id() on q and returns a *PinMismatchError if
// it does not match sqlite.Current().SourceID.
func CheckPin(ctx context.Context, q Querier) (string, error) {
	id, err := SourceID(ctx, q)
	if err != nil {
		return "", err
	}
	if err := CheckSourceID("the Go driver", id); err != nil {
		return id, err
	}
	return id, CheckDriverModules()
}

type DriverModuleMismatchError struct {
	Module string
	Got    string
	Want   string
}

func (e *DriverModuleMismatchError) Error() string {
	return fmt.Sprintf("this binary links %s %s, the pin is %s", e.Module, e.Got, e.Want)
}

func CheckDriverModules() error {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return errors.New("cannot read this binary's build info to verify the pinned driver modules")
	}
	if len(info.Deps) == 0 {
		return nil
	}
	return checkModuleVersions(info.Deps, sqlite.Current())
}

func checkModuleVersions(deps []*debug.Module, pin sqlite.Pin) error {
	wanted := []struct{ module, version string }{
		{pin.DriverModule, pin.DriverVersion},
		{pin.LibcModule, pin.LibcVersion},
	}
	for _, want := range wanted {
		if want.module == "" {
			continue
		}
		got, found := moduleVersion(deps, want.module)
		if !found {
			return &DriverModuleMismatchError{Module: want.module, Got: "(not linked)", Want: want.version}
		}
		if got != want.version {
			return &DriverModuleMismatchError{Module: want.module, Got: got, Want: want.version}
		}
	}
	return nil
}

func moduleVersion(deps []*debug.Module, path string) (string, bool) {
	for _, dep := range deps {
		if dep.Path != path {
			continue
		}
		if dep.Replace != nil {
			return dep.Replace.Version, true
		}
		return dep.Version, true
	}
	return "", false
}

// CheckSourceID compares id against sqlite.Current().SourceID and returns a
// *PinMismatchError tagged with what on a mismatch.
func CheckSourceID(what, id string) error {
	want := sqlite.Current().SourceID
	if id != want {
		return &PinMismatchError{Got: id, Want: want, What: what}
	}
	return nil
}

// PinLine formats the standard first output line printed by every experiment,
// indicating whether id equals the pinned revision.
func PinLine(id string) string {
	pin := sqlite.Current()
	if id == pin.SourceID {
		return fmt.Sprintf("sqlite_source_id: %s (pin %s ok)", id, pin.Release)
	}
	return fmt.Sprintf("sqlite_source_id: %s (NOT the pin %s, want %s)", id, pin.Release, pin.SourceID)
}

// WritePinCheck queries sqlite_source_id() on q, writes PinLine to w, and
// returns an error if the ID does not match the pin.
func WritePinCheck(ctx context.Context, w io.Writer, q Querier) error {
	id, err := SourceID(ctx, q)
	if err != nil {
		fmt.Fprintf(w, "sqlite_source_id: unavailable (%v)\n", err)
		return err
	}
	fmt.Fprintln(w, PinLine(id))
	if err := CheckSourceID("the Go driver", id); err != nil {
		return err
	}
	return CheckDriverModules()
}

// MustPinCheck writes the pin check line to stdout and exits 1 if q does not
// run the pinned SQLite revision.
func MustPinCheck(ctx context.Context, q Querier) {
	if err := WritePinCheck(ctx, os.Stdout, q); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

// MustPinCheckMemory opens a temporary in-memory database, prints the pin check
// line to stdout, and exits 1 if the Go driver does not match sqlite/PIN.
func MustPinCheckMemory(ctx context.Context) {
	db, err := openMemory(ctx)
	if err != nil {
		fmt.Println("sqlite_source_id: unavailable")
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	defer db.Close()
	MustPinCheck(ctx, db)
}
