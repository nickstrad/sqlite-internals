package trace

import (
	"fmt"
	"strings"
)

type EventKind int

const (
	EventVFS EventKind = iota
	EventSyscall
	EventOutput
)

func (k EventKind) String() string {
	switch k {
	case EventVFS:
		return "VFS"
	case EventSyscall:
		return "SYS"
	case EventOutput:
		return "OUT"
	}
	return "?"
}

type Event struct {
	Kind     EventKind
	VFS      *VFSCall
	Syscall  *Syscall
	Output   string
	Enclosed int
}

type Merged struct {
	Events []Event
	Log    *StraceLog
}

func (e Event) String() string {
	switch e.Kind {
	case EventVFS:
		return "VFS " + e.VFS.String()
	case EventSyscall:
		if e.Enclosed >= 0 {
			return "    SYS " + e.Syscall.String()
		}
		return "SYS " + e.Syscall.String()
	case EventOutput:
		return "OUT " + e.Output
	}
	return "?"
}

func MergeStdoutVFS(log *StraceLog, stdoutFD int, includeSyscall func(Syscall) bool) (*Merged, error) {
	merged := &Merged{Log: log}
	var line strings.Builder
	openEvent := -1
	lineStartsVFS := false
	for i := range log.Calls {
		call := &log.Calls[i]
		if (call.Name == "write" || call.Name == "writev") && call.FD == stdoutFD {
			if call.Truncated {
				return nil, fmt.Errorf("strace truncated a write to fd %d; raise the string limit: %s", stdoutFD, call.Raw)
			}
			data := call.Data
			for data != "" {
				if line.Len() == 0 {
					lineStartsVFS = strings.HasPrefix(data, "trace.x")
					if lineStartsVFS {
						merged.Events = append(merged.Events, Event{Kind: EventVFS, VFS: &VFSCall{Complete: false}, Enclosed: -1})
						openEvent = len(merged.Events) - 1
					}
				}
				newline := strings.IndexByte(data, '\n')
				if newline < 0 {
					line.WriteString(data)
					break
				}
				line.WriteString(data[:newline])
				data = data[newline+1:]
				text := line.String()
				line.Reset()
				if lineStartsVFS {
					parsed, ok := ParseVFSLine(text)
					if !ok {
						parsed = VFSCall{Raw: text, Method: "?"}
					}
					*merged.Events[openEvent].VFS = parsed
					openEvent = -1
				} else if !strings.HasPrefix(text, "trace.") {
					merged.Events = append(merged.Events, Event{Kind: EventOutput, Output: text, Enclosed: -1})
				}
			}
			continue
		}
		if includeSyscall != nil && !includeSyscall(*call) {
			continue
		}
		merged.Events = append(merged.Events, Event{Kind: EventSyscall, Syscall: call, Enclosed: openEvent})
	}
	if openEvent >= 0 {
		merged.Events[openEvent].VFS.Raw = line.String()
		merged.Events[openEvent].VFS.Method = "?"
	}
	vfsIndex := 0
	for _, event := range merged.Events {
		if event.Kind == EventVFS {
			event.VFS.Index = vfsIndex
			vfsIndex++
		}
	}
	return merged, nil
}

func (m *Merged) VFSCalls() []VFSCall {
	var out []VFSCall
	for _, event := range m.Events {
		if event.Kind == EventVFS {
			out = append(out, *event.VFS)
		}
	}
	return out
}

func (m *Merged) SyscallsWithin(vfsEvent int) []Syscall {
	var out []Syscall
	for _, event := range m.Events {
		if event.Kind == EventSyscall && event.Enclosed == vfsEvent {
			out = append(out, *event.Syscall)
		}
	}
	return out
}

func (m *Merged) Relative(dir string) *Merged {
	out := &Merged{Log: m.Log, Events: make([]Event, len(m.Events))}
	for i, event := range m.Events {
		switch event.Kind {
		case EventVFS:
			relative := RelativeVFS(dir, []VFSCall{*event.VFS})[0]
			event.VFS = &relative
		case EventSyscall:
			relative := RelativeTo(dir, []Syscall{*event.Syscall})[0]
			event.Syscall = &relative
		}
		out.Events[i] = event
	}
	return out
}
