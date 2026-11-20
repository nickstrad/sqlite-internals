package trace

import (
	"fmt"
	"strings"
)

// EventKind tags each item in a Merged VFS-and-syscall stream.
type EventKind int

// Event kinds produced by MergeStdoutVFS.
const (
	EventVFS EventKind = iota
	EventSyscall
	EventOutput
)

// String returns the three-letter prefix ("VFS", "SYS", or "OUT") for k.
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

// Event is one entry in a merged stream: a VFS call, an OS syscall, or a line
// of SQL query output. For EventSyscall, Enclosed holds the index of the open
// EventVFS that triggered it, or -1 if no VFS line was in progress.
type Event struct {
	Kind     EventKind
	VFS      *VFSCall
	Syscall  *Syscall
	Output   string
	Enclosed int
}

// Merged holds the ordered VFS, syscall, and output events extracted from a
// strace log of the sqlite3 CLI running with -vfstrace.
type Merged struct {
	Events []Event
	Log    *StraceLog
}

// String renders e on one line, indenting syscalls that occurred inside an open VFS call.
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

const vfsLinePrefix = "trace.x"

type stdoutLineKind int

const (
	lineUndecided stdoutLineKind = iota
	lineVFS
	lineOther
)

// MergeStdoutVFS reconstructs one ordered stream from a strace log of
// "sqlite3 -vfstrace". Because vfstrace flushes "trace.xName(args)" to stdout
// before calling the real VFS method (ext/misc/vfstrace.c:420, shell.c.in:12911)
// and flushes " -> rc\n" after it returns, syscalls observed between those two
// writes on stdoutFD belong to that VFS call. Methods that print only after
// returning (such as xOpen) leave their syscalls unenclosed (Enclosed == -1).
func MergeStdoutVFS(log *StraceLog, stdoutFD int, includeSyscall func(Syscall) bool) (*Merged, error) {
	merged := &Merged{Log: log}
	var line strings.Builder
	openEvent := -1
	lineKind := lineUndecided
	classify := func() {
		if lineKind != lineUndecided {
			return
		}
		text := line.String()
		if len(text) < len(vfsLinePrefix) && strings.HasPrefix(vfsLinePrefix, text) {
			return
		}
		if strings.HasPrefix(text, vfsLinePrefix) {
			lineKind = lineVFS
			merged.Events = append(merged.Events, Event{Kind: EventVFS, VFS: &VFSCall{Complete: false}, Enclosed: -1})
			openEvent = len(merged.Events) - 1
			return
		}
		lineKind = lineOther
	}
	for i := range log.Calls {
		call := &log.Calls[i]
		if (call.Name == "write" || call.Name == "writev") && call.FD == stdoutFD {
			if !call.Succeeded() {
				continue
			}
			if call.Truncated {
				return nil, fmt.Errorf("strace truncated a write to fd %d; raise the string limit: %s", stdoutFD, call.Raw)
			}
			data := call.Data
			if call.Result < int64(len(data)) {
				data = data[:call.Result]
			}
			for data != "" {
				newline := strings.IndexByte(data, '\n')
				if newline < 0 {
					line.WriteString(data)
					classify()
					break
				}
				line.WriteString(data[:newline])
				data = data[newline+1:]
				classify()
				if lineKind == lineUndecided {
					lineKind = lineOther
				}
				text := line.String()
				line.Reset()
				if lineKind == lineVFS {
					parsed, ok := ParseVFSLine(text)
					if !ok {
						parsed = VFSCall{Raw: text, Method: "?"}
					}
					*merged.Events[openEvent].VFS = parsed
					openEvent = -1
				} else if !strings.HasPrefix(text, "trace.") {
					merged.Events = append(merged.Events, Event{Kind: EventOutput, Output: text, Enclosed: -1})
				}
				lineKind = lineUndecided
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

// VFSCalls extracts all EventVFS entries in m in order.
func (m *Merged) VFSCalls() []VFSCall {
	var out []VFSCall
	for _, event := range m.Events {
		if event.Kind == EventVFS {
			out = append(out, *event.VFS)
		}
	}
	return out
}

// SyscallsWithin returns every syscall enclosed by the VFS event at index vfsEvent.
func (m *Merged) SyscallsWithin(vfsEvent int) []Syscall {
	var out []Syscall
	for _, event := range m.Events {
		if event.Kind == EventSyscall && event.Enclosed == vfsEvent {
			out = append(out, *event.Syscall)
		}
	}
	return out
}

// Relative returns a copy of m with dir stripped from all VFS and syscall paths.
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
