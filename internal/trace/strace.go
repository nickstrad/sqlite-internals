// Package trace captures and parses Linux syscall traces (strace), SQLite
// Virtual File System (VFS) traces from the pinned sqlite3 CLI's -vfstrace
// option, and merged streams pairing each VFS method with the syscalls it triggers.
package trace

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Lock describes a POSIX byte-range lock passed in a struct flock argument to
// fcntl (used by SQLite's Unix VFS in os_unix.c for database and -shm locks).
type Lock struct {
	Command string
	Type    string
	Whence  string
	Start   int64
	Length  int64
}

// Syscall holds one parsed system call from strace output, ordered by call entry.
// FD, Offset, and Length are -1 when not applicable to the syscall; for writev
// and readv, Length is the sum of iov_len entries. Interrupted restart results
// ("? ERESTARTSYS") record Result -1 and Errno with Unfinished false.
type Syscall struct {
	Index      int
	PID        int
	Name       string
	FD         int
	Path       string
	Offset     int64
	Length     int64
	Result     int64
	ResultText string
	Errno      string
	Flags      string
	Lock       *Lock
	Data       string
	Truncated  bool
	Unfinished bool
	Raw        string
}

// ProcessExit records one process or thread termination line ("+++ exited..."
// or "+++ killed by...") in a strace log.
type ProcessExit struct {
	PID    int
	Code   int
	Signal string
}

// StraceLog holds the ordered system calls and exit records parsed from strace output.
type StraceLog struct {
	Calls []Syscall
	Exits []ProcessExit
}

// Succeeded reports whether the syscall finished without an errno and returned >= 0.
func (s Syscall) Succeeded() bool {
	return !s.Unfinished && s.Errno == "" && s.Result >= 0
}

// String formats s as a deterministic one-line summary for experiment evidence.
// Raw descriptor numbers on open/openat ("= fd") and return addresses on mmap
// ("= ok") are normalized away because they vary across runs.
func (s Syscall) String() string {
	var b strings.Builder
	b.WriteString(s.Name)
	if s.Path != "" {
		b.WriteString(" ")
		b.WriteString(s.Path)
	} else if s.FD >= 0 {
		fmt.Fprintf(&b, " fd=%d", s.FD)
	}
	switch s.Name {
	case "pwrite64", "pread64", "pwrite", "pread":
		fmt.Fprintf(&b, " off=%d len=%d", s.Offset, s.Length)
	case "write", "read", "writev", "readv":
		fmt.Fprintf(&b, " len=%d", s.Length)
	case "ftruncate":
		fmt.Fprintf(&b, " len=%d", s.Length)
	case "mmap":
		fmt.Fprintf(&b, " off=%d len=%d", s.Offset, s.Length)
	case "openat", "open":
		b.WriteString(" " + s.Flags)
	case "fcntl":
		if s.Lock != nil {
			fmt.Fprintf(&b, " %s %s start=%d len=%d", s.Lock.Command, s.Lock.Type, s.Lock.Start, s.Lock.Length)
		} else if s.Flags != "" {
			b.WriteString(" " + s.Flags)
		}
	}
	switch {
	case s.Unfinished:
		b.WriteString(" = ? (process killed)")
	case s.Errno != "":
		fmt.Fprintf(&b, " = -1 %s", s.Errno)
	case s.Name == "mmap":
		b.WriteString(" = ok")
	case s.Name == "openat" || s.Name == "open":
		b.WriteString(" = fd")
	default:
		fmt.Fprintf(&b, " = %d", s.Result)
	}
	return b.String()
}

var (
	pidPrefix    = regexp.MustCompile(`^(\d+)\s+(.*)$`)
	callStart    = regexp.MustCompile(`^(\w+)\((.*)$`)
	resumedStart = regexp.MustCompile(`^<\.\.\. (\w+) resumed>(.*)$`)
	exitedLine   = regexp.MustCompile(`^\+\+\+ exited with (\d+) \+\+\+$`)
	killedLine   = regexp.MustCompile(`^\+\+\+ killed by (SIG\w+)(?: \(core dumped\))? \+\+\+$`)
	resultPart   = regexp.MustCompile(`^(.*)\)\s+=\s+(.*)$`)
	fdWithPath   = regexp.MustCompile(`^(-?\d+|AT_FDCWD)(?:<(.*)>)?(?:\(deleted\))?$`)
	interrupted  = regexp.MustCompile(`^\?\s+(E[A-Z0-9]+)`)
	iovecLen     = regexp.MustCompile(`iov_len=(\d+)`)
	quotedString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	errnoResult  = regexp.MustCompile(`^-1 (E[A-Z0-9]+)`)
	iovecBase    = regexp.MustCompile(`iov_base=("(?:[^"\\]|\\.)*")(\.\.\.)?`)
	lockStruct   = regexp.MustCompile(`l_type=(\w+), l_whence=(\w+), l_start=(-?\d+), l_len=(-?\d+)`)
)

const unfinishedMarker = " <unfinished ...>"

type pendingCall struct {
	index int
	name  string
	args  string
}

type straceParser struct {
	log     StraceLog
	pending map[int]pendingCall
	fdPaths map[int]string
}

// ParseStrace reads strace output produced with "-f" (pid-prefixed lines,
// optionally with "-y" path annotations including "(deleted)" suffixes),
// rejoining split unfinished/resumed calls at their entry index and marking
// calls interrupted by a kill as Unfinished. When "-y" annotations are absent,
// one shared fd-to-path table tracks open/close across all traced threads.
func ParseStrace(r io.Reader) (*StraceLog, error) {
	parser := &straceParser{pending: map[int]pendingCall{}, fdPaths: map[int]string{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		if err := parser.line(scanner.Text()); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for pid, pending := range parser.pending {
		call := &parser.log.Calls[pending.index]
		call.Unfinished = true
		call.PID = pid
	}
	return &parser.log, nil
}

// ParseStraceText parses strace output from a string.
func ParseStraceText(text string) (*StraceLog, error) {
	return ParseStrace(strings.NewReader(text))
}

func (p *straceParser) line(raw string) error {
	match := pidPrefix.FindStringSubmatch(raw)
	if match == nil {
		return nil
	}
	pid, _ := strconv.Atoi(match[1])
	body := match[2]
	if m := exitedLine.FindStringSubmatch(body); m != nil {
		code, _ := strconv.Atoi(m[1])
		p.log.Exits = append(p.log.Exits, ProcessExit{PID: pid, Code: code})
		p.finishKilled(pid)
		return nil
	}
	if m := killedLine.FindStringSubmatch(body); m != nil {
		p.log.Exits = append(p.log.Exits, ProcessExit{PID: pid, Code: -1, Signal: m[1]})
		p.finishKilled(pid)
		return nil
	}
	if strings.HasPrefix(body, "---") || strings.HasPrefix(body, "+++") {
		return nil
	}
	if m := resumedStart.FindStringSubmatch(body); m != nil {
		pending, ok := p.pending[pid]
		if !ok || pending.name != m[1] {
			return nil
		}
		delete(p.pending, pid)
		args, result, ok := splitResult(m[2])
		if !ok {
			return nil
		}
		joined := joinSplitArgs(pending.args, args)
		call := &p.log.Calls[pending.index]
		call.Raw += " | " + raw
		p.fill(call, joined, result)
		return nil
	}
	m := callStart.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	name, rest := m[1], m[2]
	call := Syscall{Index: len(p.log.Calls), PID: pid, Name: name, FD: -1, Offset: -1, Length: -1, Raw: raw}
	if strings.HasSuffix(rest, unfinishedMarker) {
		args := strings.TrimSuffix(rest, unfinishedMarker)
		p.log.Calls = append(p.log.Calls, call)
		p.pending[pid] = pendingCall{index: call.Index, name: name, args: args}
		p.fillArgs(&p.log.Calls[call.Index], args)
		return nil
	}
	args, result, ok := splitResult(rest)
	if !ok {
		return nil
	}
	p.log.Calls = append(p.log.Calls, call)
	p.fill(&p.log.Calls[call.Index], args, result)
	return nil
}

// joinSplitArgs rejoins the two halves of an <unfinished ...> / <... resumed>
// argument list without doubling a trailing comma left before the split.
func joinSplitArgs(before, after string) string {
	before = strings.TrimRight(before, ", ")
	after = strings.TrimLeft(after, ", ")
	switch {
	case before == "":
		return after
	case after == "":
		return before
	}
	return before + ", " + after
}

func (p *straceParser) finishKilled(pid int) {
	if pending, ok := p.pending[pid]; ok {
		p.log.Calls[pending.index].Unfinished = true
		delete(p.pending, pid)
	}
}

func splitResult(rest string) (string, string, bool) {
	m := resultPart.FindStringSubmatch(rest)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

func (p *straceParser) fill(call *Syscall, args, result string) {
	p.fillArgs(call, args)
	call.ResultText = result
	if result == "?" {
		call.Unfinished = true
		return
	}
	if m := interrupted.FindStringSubmatch(result); m != nil {
		call.Result = -1
		call.Errno = m[1]
		return
	}
	if m := errnoResult.FindStringSubmatch(result); m != nil {
		call.Result = -1
		call.Errno = m[1]
		return
	}
	value := result
	if i := strings.IndexAny(value, " <"); i >= 0 {
		value = value[:i]
	}
	if strings.HasPrefix(value, "0x") {
		number, _ := strconv.ParseUint(value[2:], 16, 64)
		call.Result = int64(number)
	} else {
		call.Result, _ = strconv.ParseInt(value, 10, 64)
	}
	switch call.Name {
	case "openat", "open":
		if call.Result >= 0 {
			call.FD = int(call.Result)
			p.setFD(call.FD, call.Path)
		}
	case "close":
		if call.Result == 0 && call.FD >= 0 {
			p.clearFD(call.FD)
		}
	}
}

func (p *straceParser) setFD(fd int, path string) {
	p.fdPaths[fd] = path
}

func (p *straceParser) clearFD(fd int) {
	delete(p.fdPaths, fd)
}

func (p *straceParser) pathForFD(fd int) string {
	return p.fdPaths[fd]
}

func (p *straceParser) parseFD(call *Syscall, arg string) {
	m := fdWithPath.FindStringSubmatch(strings.TrimSpace(arg))
	if m == nil {
		return
	}
	if m[1] == "AT_FDCWD" {
		return
	}
	call.FD, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		call.Path = m[2]
	} else {
		call.Path = p.pathForFD(call.FD)
	}
}

func atDirPath(arg string) string {
	m := fdWithPath.FindStringSubmatch(strings.TrimSpace(arg))
	if m == nil {
		return ""
	}
	return m[2]
}

func parseInt(arg string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(arg), 0, 64)
	if err != nil {
		return -1
	}
	return value
}

func (p *straceParser) fillArgs(call *Syscall, argText string) {
	args := SplitArgs(argText)
	at := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	switch call.Name {
	case "openat":
		path, truncated := Unquote(at(1))
		call.Truncated = truncated
		if !filepath.IsAbs(path) {
			if dir := atDirPath(at(0)); dir != "" {
				path = filepath.Join(dir, path)
			}
		}
		call.Path = path
		call.Flags = at(2)
	case "open":
		call.Path, call.Truncated = Unquote(at(0))
		call.Flags = at(1)
	case "unlink":
		call.Path, call.Truncated = Unquote(at(0))
	case "unlinkat":
		path, truncated := Unquote(at(1))
		call.Truncated = truncated
		if !filepath.IsAbs(path) {
			if dir := atDirPath(at(0)); dir != "" {
				path = filepath.Join(dir, path)
			}
		}
		call.Path = path
		call.Flags = at(2)
	case "pwrite64", "pread64", "pwrite", "pread":
		p.parseFD(call, at(0))
		call.Truncated = strings.HasSuffix(at(1), "...")
		call.Length = parseInt(at(2))
		call.Offset = parseInt(at(3))
	case "write", "read":
		p.parseFD(call, at(0))
		call.Data, call.Truncated = Unquote(at(1))
		call.Length = parseInt(at(2))
	case "writev", "readv":
		p.parseFD(call, at(0))
		var data strings.Builder
		for _, m := range iovecBase.FindAllStringSubmatch(at(1), -1) {
			part, truncated := Unquote(m[1] + m[2])
			data.WriteString(part)
			call.Truncated = call.Truncated || truncated
		}
		if strings.HasSuffix(strings.TrimSpace(at(1)), "...]") {
			call.Truncated = true
		}
		call.Data = data.String()
		if lengths := iovecLen.FindAllStringSubmatch(quotedString.ReplaceAllString(at(1), `""`), -1); lengths != nil {
			call.Length = 0
			for _, m := range lengths {
				n, _ := strconv.ParseInt(m[1], 10, 64)
				call.Length += n
			}
		}
	case "ftruncate":
		p.parseFD(call, at(0))
		call.Length = parseInt(at(1))
	case "mmap":
		call.Length = parseInt(at(1))
		call.Flags = at(3)
		p.parseFD(call, at(4))
		call.Offset = parseInt(at(5))
		// Anonymous mappings pass fd -1 and have no backing file.
		if call.FD < 0 {
			call.Path = ""
		}
	case "fcntl":
		p.parseFD(call, at(0))
		call.Flags = at(1)
		if m := lockStruct.FindStringSubmatch(at(2)); m != nil {
			start, _ := strconv.ParseInt(m[3], 10, 64)
			length, _ := strconv.ParseInt(m[4], 10, 64)
			call.Lock = &Lock{Command: at(1), Type: m[1], Whence: m[2], Start: start, Length: length}
		}
	default:
		if len(args) > 0 {
			p.parseFD(call, at(0))
		}
	}
}

// SplitArgs splits a strace or vfstrace argument list at top-level commas,
// ignoring commas inside quoted strings, nested braces/brackets/parentheses,
// and strace -y path annotations like 3</dir, name/file> or AT_FDCWD</dir, name>.
func SplitArgs(text string) []string {
	var args []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inQuote {
			if c == '\\' {
				i++
			} else if c == '"' {
				inQuote = false
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case '<':
			// Skip across fd<path> or AT_FDCWD<path> so commas inside paths do not split args.
			if i > 0 && text[i-1] >= '0' && text[i-1] <= '9' || i > 0 && text[i-1] == 'D' {
				end := strings.IndexByte(text[i:], '>')
				if end > 0 {
					i += end
				}
			}
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(text[start:i]))
				start = i + 1
			}
		}
	}
	if tail := strings.TrimSpace(text[start:]); tail != "" || len(args) > 0 {
		args = append(args, tail)
	}
	return args
}

// Unquote decodes a strace C-escaped string literal and reports whether a
// trailing "..." indicates that strace truncated the buffer at its -s limit.
func Unquote(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	truncated := strings.HasSuffix(arg, "...")
	arg = strings.TrimSuffix(arg, "...")
	if len(arg) < 2 || arg[0] != '"' || arg[len(arg)-1] != '"' {
		return arg, truncated
	}
	body := arg[1 : len(arg)-1]
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' || i+1 >= len(body) {
			out.WriteByte(c)
			continue
		}
		i++
		switch e := body[i]; e {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case 'v':
			out.WriteByte('\v')
		case 'f':
			out.WriteByte('\f')
		case '"', '\\':
			out.WriteByte(e)
		case 'x':
			end := i + 1
			for end < len(body) && end < i+3 && isHex(body[end]) {
				end++
			}
			value, _ := strconv.ParseUint(body[i+1:end], 16, 8)
			out.WriteByte(byte(value))
			i = end - 1
		default:
			if e >= '0' && e <= '7' {
				end := i
				for end < len(body) && end < i+3 && body[end] >= '0' && body[end] <= '7' {
					end++
				}
				value, _ := strconv.ParseUint(body[i:end], 8, 16)
				out.WriteByte(byte(value))
				i = end - 1
			} else {
				out.WriteByte('\\')
				out.WriteByte(e)
			}
		}
	}
	return out.String(), truncated
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// Filter returns every syscall in l for which keep returns true.
func (l *StraceLog) Filter(keep func(Syscall) bool) []Syscall {
	var out []Syscall
	for _, call := range l.Calls {
		if keep(call) {
			out = append(out, call)
		}
	}
	return out
}

// UnderDir returns a Filter predicate matching syscalls whose Path is dir itself
// (such as a directory fsync) or any file inside dir.
func UnderDir(dir string) func(Syscall) bool {
	clean := filepath.Clean(dir)
	return func(call Syscall) bool {
		return call.Path == clean || strings.HasPrefix(call.Path, clean+string(filepath.Separator))
	}
}

// Named returns a Filter predicate matching syscalls whose Name is in names.
func Named(names ...string) func(Syscall) bool {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return func(call Syscall) bool { return set[call.Name] }
}

// Both returns a Filter predicate that succeeds only when both a and b succeed.
func Both(a, b func(Syscall) bool) func(Syscall) bool {
	return func(call Syscall) bool { return a(call) && b(call) }
}

// RelativeTo returns a copy of calls with dir stripped from each Path (dir
// itself becomes ".") so trace output does not contain machine-local paths.
func RelativeTo(dir string, calls []Syscall) []Syscall {
	clean := filepath.Clean(dir)
	out := make([]Syscall, len(calls))
	for i, call := range calls {
		switch {
		case call.Path == clean:
			call.Path = "."
		case strings.HasPrefix(call.Path, clean+string(filepath.Separator)):
			call.Path = call.Path[len(clean)+1:]
		}
		out[i] = call
	}
	return out
}

// KilledBy returns the first terminating signal recorded in l.Exits, or "".
func (l *StraceLog) KilledBy() string {
	for _, exit := range l.Exits {
		if exit.Signal != "" {
			return exit.Signal
		}
	}
	return ""
}
