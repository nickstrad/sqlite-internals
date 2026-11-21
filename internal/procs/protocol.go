// Package procs coordinates scripted multi-process SQLite experiments over
// step barriers. An experiment binary re-executes itself in named child roles
// ("sql", "copier", or custom roles), exchanging JSON request/reply lines over
// inherited pipes (file descriptors 3 and 4) so lock ownership and interleavings
// are deterministic rather than timing-dependent.
package procs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// ChildMarker is the os.Args[1] sentinel that switches a re-executed binary
// into child-role mode inside Serve.
const ChildMarker = "procs-child"

// Control actions exchanged between parent and child outside role-defined actions.
const (
	// ActionReady is the step-0 reply a child emits once its role has started, carrying its PID.
	ActionReady = "ready"
	// ActionExit asks a child to close its Actor, reply with the close result, and exit.
	ActionExit = "exit"
)

// Result code sentinels stored in Result.Code.
const (
	// CodeOK indicates a command succeeded (matching SQLITE_OK).
	CodeOK = 0
	// CodeNotSQLite marks an error that did not originate from SQLite (no SQLite result code).
	CodeNotSQLite = -1
)

// Inherited pipe file descriptors attached via exec.Cmd.ExtraFiles in StartCommand.
// Go maps ExtraFiles[0] to fd 3 and ExtraFiles[1] to fd 4, keeping stdout/stderr free for diagnostics.
const (
	requestFD = 3
	replyFD   = 4
)

// Request is one numbered step sent from the parent to a child over fd 3.
type Request struct {
	Step   int      `json:"step"`
	Action string   `json:"action"`
	Args   []string `json:"args,omitempty"`
}

// Result is the child's reply on fd 4 for a Request, recording either a value
// on success or a SQLite extended result code (or CodeNotSQLite) on failure.
type Result struct {
	Step   int    `json:"step"`
	Action string `json:"action"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
	Value  string `json:"value,omitempty"`
}

// OK reports whether the step succeeded with CodeOK and no error string.
func (r Result) OK() bool { return r.Code == CodeOK && r.Error == "" }

// CodeName returns the SQLite macro name for r.Code, or "ERROR" for non-SQLite errors.
func (r Result) CodeName() string {
	if r.Code == CodeNotSQLite {
		return "ERROR"
	}
	return sqlitex.ResultCodeName(r.Code)
}

// Outcome formats r as a single transcript token: "SQLITE_OK [value]",
// "<SQLITE_NAME> (<code>)", or "ERROR <text>".
func (r Result) Outcome() string {
	if r.OK() {
		if r.Value == "" {
			return sqlitex.ResultCodeName(CodeOK)
		}
		return sqlitex.ResultCodeName(CodeOK) + " " + r.Value
	}
	if r.Code == CodeNotSQLite {
		return "ERROR " + r.Error
	}
	return fmt.Sprintf("%s (%d)", r.CodeName(), r.Code)
}

// Handler executes one named action inside a child process and returns an
// optional single-line text value or an error.
type Handler func(ctx context.Context, args []string) (string, error)

// Actor holds the action handlers and optional cleanup hook for a started role.
type Actor struct {
	Actions map[string]Handler
	Close   func() error
}

// Role constructs an Actor from role-specific command-line arguments when a
// child process starts.
type Role func(ctx context.Context, args []string) (*Actor, error)

// ResultFor converts a Handler's (value, err) return into a Result, extracting
// SQLite's extended result code when err wraps a modernc.org/sqlite error.
func ResultFor(request Request, value string, err error) Result {
	result := Result{Step: request.Step, Action: request.Action, Value: value}
	if err == nil {
		return result
	}
	result.Error = err.Error()
	if code, ok := sqlitex.ErrorCode(err); ok {
		result.Code = code
	} else {
		result.Code = CodeNotSQLite
	}
	return result
}

// ErrUnknownRole is returned when RunChild is asked to start an unregistered role name.
var ErrUnknownRole = errors.New("procs: unknown role")

// Serve checks whether the current process was re-executed as a procs child
// (os.Args[1] == ChildMarker). If so, it runs the requested role over fd 3 and
// fd 4 and terminates the process; otherwise it returns false so main or
// TestMain can continue as the parent.
func Serve(roles map[string]Role) bool {
	if len(os.Args) < 3 || os.Args[1] != ChildMarker {
		return false
	}
	requests := os.NewFile(requestFD, "procs-requests")
	replies := os.NewFile(replyFD, "procs-replies")
	if requests == nil || replies == nil {
		fmt.Fprintln(os.Stderr, "procs: child started without its request and reply pipes")
		os.Exit(3)
	}
	err := RunChild(context.Background(), roles, os.Args[2], os.Args[3:], requests, replies)
	if err != nil {
		fmt.Fprintf(os.Stderr, "procs: child %s: %v\n", os.Args[2], err)
		os.Exit(2)
	}
	os.Exit(0)
	return true
}

// RunChild starts roleName, sends a step-0 ready reply containing the child's
// PID, and processes newline-delimited JSON requests until ActionExit arrives.
// Closing requests before ActionExit is treated as an error so a child detects
// a crashed parent instead of exiting silently.
func RunChild(ctx context.Context, roles map[string]Role, roleName string, args []string, requests io.Reader, replies io.Writer) error {
	role, ok := roles[roleName]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownRole, roleName)
	}
	encoder := json.NewEncoder(replies)
	actor, err := role(ctx, args)
	if err != nil {
		return fmt.Errorf("start role: %w", err)
	}
	if actor == nil {
		return errors.New("role returned no actor")
	}
	ready := Result{Step: 0, Action: ActionReady, Value: strconv.Itoa(os.Getpid())}
	if err := encoder.Encode(ready); err != nil {
		closeActor(actor)
		return err
	}
	scanner := bufio.NewScanner(requests)
	// Allow large SQL arguments or values without hitting bufio.Scanner's 64 KiB default cap.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var request Request
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			closeActor(actor)
			return fmt.Errorf("malformed request %q: %w", scanner.Text(), err)
		}
		if request.Action == ActionExit {
			closeErr := closeActor(actor)
			result := ResultFor(request, "", closeErr)
			if err := encoder.Encode(result); err != nil {
				return err
			}
			return closeErr
		}
		handler, ok := actor.Actions[request.Action]
		var result Result
		if !ok {
			result = ResultFor(request, "", fmt.Errorf("role %s has no action %q", roleName, request.Action))
		} else {
			value, err := handler(ctx, request.Args)
			result = ResultFor(request, value, err)
		}
		if err := encoder.Encode(result); err != nil {
			closeActor(actor)
			return err
		}
	}
	closeErr := closeActor(actor)
	if err := scanner.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return errors.New("request pipe closed before an exit request")
}

func closeActor(actor *Actor) error {
	if actor.Close == nil {
		return nil
	}
	return actor.Close()
}
