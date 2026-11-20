package procs

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type Script struct {
	Out     io.Writer
	Timeout time.Duration

	step     int
	children []*Child
}

func NewScript(out io.Writer) *Script {
	return &Script{Out: out, Timeout: DefaultTimeout}
}

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

func (s *Script) Children() []*Child { return append([]*Child(nil), s.children...) }

func (s *Script) NameOfPid(pid int) (string, bool) {
	for _, child := range s.children {
		if child.Pid == pid {
			return child.Name, true
		}
	}
	return "", false
}

func (s *Script) Step() int { return s.step }

func (s *Script) Do(child *Child, action string, args ...string) (Result, error) {
	call, err := s.Begin(child, action, args...)
	if err != nil {
		return Result{}, err
	}
	return s.Finish(call)
}

func (s *Script) Begin(child *Child, action string, args ...string) (*Call, error) {
	s.step++
	call, err := child.Start(s.step, action, args...)
	if err != nil {
		return nil, err
	}
	return call, nil
}

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

func ActionText(action string, args []string) string {
	if len(args) == 0 {
		return action
	}
	return action + " " + strings.Join(args, " ")
}

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

func (s *Script) Kill() {
	for _, child := range s.children {
		child.Kill()
	}
	s.children = nil
}
