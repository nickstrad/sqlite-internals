package procs

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Script tracks a set of named Child processes, assigns global step numbers
// across them, and prints a deterministic transcript line as each step finishes.
type Script struct {
	Out     io.Writer
	Timeout time.Duration

	step     int
	children []*Child
}

// NewScript returns a Script writing transcript lines to out with DefaultTimeout.
func NewScript(out io.Writer) *Script {
	return &Script{Out: out, Timeout: DefaultTimeout}
}

// Spawn starts a child in role under a unique logical name and registers it on s.
func (s *Script) Spawn(name, role string, args ...string) (*Child, error) {
	for _, existing := range s.children {
		if existing.Name == name {
			return nil, fmt.Errorf("procs: a child named %q already exists", name)
		}
	}
	child, err := StartChild(name, role, args...)
	if err != nil {
		return nil, err
	}
	child.Timeout = s.Timeout
	s.children = append(s.children, child)
	return child, nil
}

// Children returns a shallow copy of the currently registered children.
func (s *Script) Children() []*Child { return append([]*Child(nil), s.children...) }

// NameOfPid maps a child OS PID back to its logical name in s so lock probes
// and /proc/locks listings print stable names instead of ephemeral PIDs.
func (s *Script) NameOfPid(pid int) (string, bool) {
	for _, child := range s.children {
		if child.Pid == pid {
			return child.Name, true
		}
	}
	return "", false
}

// Step returns the most recently assigned global step number.
func (s *Script) Step() int { return s.step }

// Do sends action to child, waits for its reply, and prints the transcript line.
func (s *Script) Do(child *Child, action string, args ...string) (Result, error) {
	call, err := s.Begin(child, action, args...)
	if err != nil {
		return Result{}, err
	}
	return s.Finish(call)
}

// Begin increments the global step counter and sends action to child without
// waiting for the reply.
func (s *Script) Begin(child *Child, action string, args ...string) (*Call, error) {
	s.step++
	call, err := child.Start(s.step, action, args...)
	if err != nil {
		return nil, err
	}
	return call, nil
}

// Finish waits for a Call started by Begin and prints its transcript line.
func (s *Script) Finish(call *Call) (Result, error) {
	result, err := call.Wait()
	if err != nil {
		return result, err
	}
	s.print(call, result)
	return result, nil
}

func (s *Script) print(call *Call, result Result) {
	if s.Out == nil {
		return
	}
	request := call.Request()
	fmt.Fprintf(s.Out, "step %2d  %-10s %-36s -> %s\n", request.Step, call.child.Name, ActionText(request.Action, request.Args), result.Outcome())
}

// ActionText formats an action name and its arguments for transcript output.
func ActionText(action string, args []string) string {
	if len(args) == 0 {
		return action
	}
	return action + " " + strings.Join(args, " ")
}

// Close stops every child in reverse spawn order so later connections close
// before earlier ones, joining any close errors.
func (s *Script) Close() error {
	var errs []error
	for i := len(s.children) - 1; i >= 0; i-- {
		if err := s.children[i].Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	s.children = nil
	return errors.Join(errs...)
}

// Kill forcefully terminates every child tracked by s.
func (s *Script) Kill() {
	for _, child := range s.children {
		child.Kill()
	}
	s.children = nil
}
