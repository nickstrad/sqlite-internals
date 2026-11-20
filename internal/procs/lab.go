package procs

import (
	"fmt"
	"io"
	"os"
)

type Lab struct {
	Out      io.Writer
	Script   *Script
	failures []string
	exit     func(int)
}

func NewLab(out io.Writer) *Lab {
	return &Lab{Out: out, Script: NewScript(out), exit: os.Exit}
}

func (l *Lab) Fatal(what string, err error) {
	l.Script.Kill()
	fmt.Fprintf(l.Out, "FATAL %s: %v\n", what, err)
	l.exit(1)
}

func (l *Lab) Check(ok bool, claim string) bool {
	status := "ok"
	if !ok {
		status = "FAILED"
		l.failures = append(l.failures, claim)
	}
	fmt.Fprintf(l.Out, "check %s: %s\n", status, claim)
	return ok
}

func (l *Lab) Failures() []string { return append([]string(nil), l.failures...) }

func (l *Lab) Spawn(name, role string, args ...string) *Child {
	child, err := l.Script.Spawn(name, role, args...)
	if err != nil {
		l.Fatal("spawn "+name, err)
	}
	return child
}

func (l *Lab) SpawnSQL(name string, options SQLOptions) *Child {
	return l.Spawn(name, RoleSQL, options.Args()...)
}

func (l *Lab) Do(child *Child, action string, args ...string) Result {
	result, err := l.Script.Do(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	return result
}

func (l *Lab) Must(child *Child, action string, args ...string) Result {
	result := l.Do(child, action, args...)
	if !result.OK() {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), fmt.Errorf("%s %s: %s %s", child.Name, ActionText(action, args), result.Outcome(), result.Error))
	}
	return result
}

func (l *Lab) Begin(child *Child, action string, args ...string) *Call {
	call, err := l.Script.Begin(child, action, args...)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", l.Script.Step()), err)
	}
	request := call.Request()
	fmt.Fprintf(l.Out, "step %2d  %-10s %-36s .. sent, not waiting for the answer\n", request.Step, child.Name, ActionText(request.Action, request.Args))
	return call
}

func (l *Lab) StillWaiting(call *Call) bool {
	answered, err := call.Answered()
	if err != nil {
		l.Fatal("poll", err)
	}
	request := call.Request()
	state := "no answer yet"
	if answered {
		state = "already answered"
	}
	fmt.Fprintf(l.Out, "step %2d  %-10s %-36s .. %s\n", request.Step, call.child.Name, ActionText(request.Action, request.Args), state)
	return !answered
}

func (l *Lab) Finish(call *Call) Result {
	result, err := l.Script.Finish(call)
	if err != nil {
		l.Fatal(fmt.Sprintf("step %d", call.Request().Step), err)
	}
	return result
}

func (l *Lab) StopChildren() {
	if err := l.Script.Close(); err != nil {
		l.Fatal("stop children", err)
	}
}

func (l *Lab) PidName(pid int) string {
	if name, ok := l.Script.NameOfPid(pid); ok {
		return name
	}
	return "another process"
}

func (l *Lab) Locks(label, path string, ranges []LockRange) []RangeState {
	states, err := ProbeRanges(path, ranges)
	if err != nil {
		l.Fatal("probe locks", err)
	}
	fmt.Fprintf(l.Out, "  %s locks (fcntl F_GETLK from the parent):\n", label)
	for _, line := range FormatRangeStates(states, l.PidName) {
		fmt.Fprintf(l.Out, "    %s\n", line)
	}
	if ProcLocksSupported {
		l.procLocks(label, path, ranges)
	}
	return states
}

func (l *Lab) procLocks(label, path string, ranges []LockRange) {
	inode, err := FileInode(path)
	if err != nil {
		l.Fatal("inode", err)
	}
	all, err := ReadProcLocks()
	if err != nil {
		l.Fatal(ProcLocksPath, err)
	}
	names := map[int]string{}
	pids := map[int]bool{}
	for _, child := range l.Script.Children() {
		names[child.Pid] = child.Name
		pids[child.Pid] = true
	}
	lines := DescribeProcLocks(FilterProcLocks(all, inode, pids), names, ranges)
	if len(lines) == 0 {
		fmt.Fprintf(l.Out, "  %s locks in %s: none\n", label, ProcLocksPath)
		return
	}
	fmt.Fprintf(l.Out, "  %s locks in %s:\n", label, ProcLocksPath)
	for _, line := range lines {
		fmt.Fprintf(l.Out, "    %s\n", line)
	}
}

func StateOf(states []RangeState, name string) (LockHolder, bool) {
	for _, s := range states {
		if s.Range.Name == name {
			return s.Holder, true
		}
	}
	return LockHolder{}, false
}

func WriteLockedBy(states []RangeState, name string, pid int) bool {
	holder, ok := StateOf(states, name)
	return ok && holder.Held && holder.Write && holder.Pid == pid
}

func ReadLocked(states []RangeState, name string) bool {
	holder, ok := StateOf(states, name)
	return ok && holder.Held && !holder.Write
}

func Unlocked(states []RangeState, names ...string) bool {
	for _, s := range states {
		if len(names) > 0 && !contains(names, s.Range.Name) {
			continue
		}
		if s.Holder.Held {
			return false
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func (l *Lab) Report(title string) {
	fmt.Fprintln(l.Out)
	if len(l.failures) > 0 {
		fmt.Fprintf(l.Out, "%s FAILED: %d claim(s) did not hold\n", title, len(l.failures))
		for _, f := range l.failures {
			fmt.Fprintf(l.Out, "  - %s\n", f)
		}
		l.exit(1)
		return
	}
	fmt.Fprintf(l.Out, "%s OK\n", title)
}
