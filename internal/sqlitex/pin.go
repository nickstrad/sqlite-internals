package sqlitex

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickstrad/sqlite-internals/sqlite"
)

type PinMismatchError struct {
	Got  string
	Want string
	What string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("%s reports sqlite_source_id %q, the pin is %q", e.What, e.Got, e.Want)
}

func SourceID(ctx context.Context, q Querier) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, "SELECT sqlite_source_id()").Scan(&id)
	return id, err
}

func CheckPin(ctx context.Context, q Querier) (string, error) {
	id, err := SourceID(ctx, q)
	if err != nil {
		return "", err
	}
	return id, CheckSourceID("the Go driver", id)
}

func CheckSourceID(what, id string) error {
	want := sqlite.Current().SourceID
	if id != want {
		return &PinMismatchError{Got: id, Want: want, What: what}
	}
	return nil
}

func PinLine(id string) string {
	pin := sqlite.Current()
	if id == pin.SourceID {
		return fmt.Sprintf("sqlite_source_id: %s (pin %s ok)", id, pin.Release)
	}
	return fmt.Sprintf("sqlite_source_id: %s (NOT the pin %s, want %s)", id, pin.Release, pin.SourceID)
}

func WritePinCheck(ctx context.Context, w io.Writer, q Querier) error {
	id, err := SourceID(ctx, q)
	if err != nil {
		fmt.Fprintf(w, "sqlite_source_id: unavailable (%v)\n", err)
		return err
	}
	fmt.Fprintln(w, PinLine(id))
	return CheckSourceID("the Go driver", id)
}

func MustPinCheck(ctx context.Context, q Querier) {
	if err := WritePinCheck(ctx, os.Stdout, q); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

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
