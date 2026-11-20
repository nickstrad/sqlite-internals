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

const ChildMarker = "procs-child"

const (
	ActionReady = "ready"
	ActionExit  = "exit"
)

const (
	CodeOK        = 0
	CodeNotSQLite = -1
)

const (
	requestFD = 3
	replyFD   = 4
)

type Request struct {
	Step   int      `json:"step"`
	Action string   `json:"action"`
	Args   []string `json:"args,omitempty"`
}

type Result struct {
	Step   int    `json:"step"`
	Action string `json:"action"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
	Value  string `json:"value,omitempty"`
}

func (r Result) OK() bool { return r.Code == CodeOK && r.Error == "" }

func (r Result) CodeName() string {
	if r.Code == CodeNotSQLite {
		return "ERROR"
	}
	return sqlitex.ResultCodeName(r.Code)
}

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

type Handler func(ctx context.Context, args []string) (string, error)

type Actor struct {
	Actions map[string]Handler
	Close   func() error
}

type Role func(ctx context.Context, args []string) (*Actor, error)

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

var ErrUnknownRole = errors.New("procs: unknown role")

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
