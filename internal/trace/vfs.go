package trace

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// VFSCall represents one method call logged by SQLite's vfstrace shim
// (ext/misc/vfstrace.c in the pinned tree). Amount and Offset are -1 when absent.
type VFSCall struct {
	Index    int
	Method   string
	File     string
	Args     []string
	Amount   int64
	Offset   int64
	Flag     string
	ShmSlot  string
	Result   string
	Detail   string
	Complete bool
	Raw      string
}

var (
	vfsCallLine = regexp.MustCompile(`^(\w+)\.(x\w+)\((.*)\)(?: -> (.*))?$`)
	vfsNumber   = regexp.MustCompile(`^(\w+)=(-?\d+)$`)
	vfsShmOfst  = regexp.MustCompile(`^ofst=(\d+)\((\w+)\)$`)
)

// String formats c without the leading "trace." prefix for compact evidence listings.
func (c VFSCall) String() string {
	var b strings.Builder
	b.WriteString(c.Method)
	b.WriteString("(")
	b.WriteString(strings.Join(c.Args, ","))
	b.WriteString(")")
	if c.Result != "" {
		b.WriteString(" -> ")
		b.WriteString(c.Result)
		if c.Detail != "" {
			b.WriteString(", ")
			b.WriteString(c.Detail)
		}
	}
	return b.String()
}

// ParseVFSLine parses one line of the form "trace.xMethod(args) -> result[, detail]"
// (or without " -> ..." for void methods such as xShmBarrier and xRandomness).
func ParseVFSLine(line string) (VFSCall, bool) {
	line = strings.TrimRight(line, "\r\n")
	m := vfsCallLine.FindStringSubmatch(line)
	if m == nil {
		return VFSCall{}, false
	}
	call := VFSCall{Method: m[2], Amount: -1, Offset: -1, Raw: line, Complete: true}
	call.Args = SplitArgs(m[3])
	if len(call.Args) > 0 {
		first, _ := Unquote(call.Args[0])
		call.File = first
	}
	if m[4] != "" {
		result, detail, _ := strings.Cut(m[4], ",")
		call.Result = strings.TrimSpace(result)
		call.Detail = strings.TrimSpace(detail)
	}
	for _, arg := range call.Args[min(1, len(call.Args)):] {
		if s := vfsShmOfst.FindStringSubmatch(arg); s != nil {
			call.Offset, _ = strconv.ParseInt(s[1], 10, 64)
			call.ShmSlot = s[2]
			continue
		}
		if n := vfsNumber.FindStringSubmatch(arg); n != nil {
			value, _ := strconv.ParseInt(n[2], 10, 64)
			switch n[1] {
			case "n":
				call.Amount = value
			case "ofst":
				call.Offset = value
			}
			continue
		}
	}
	switch call.Method {
	case "xSync", "xLock", "xUnlock":
		if len(call.Args) > 1 {
			call.Flag = call.Args[1]
		}
	case "xFileControl":
		if len(call.Args) > 1 {
			call.Flag = call.Args[1]
		}
	case "xShmLock":
		if len(call.Args) > 3 {
			call.Flag = call.Args[3]
		}
	case "xTruncate":
		if len(call.Args) > 1 {
			call.Amount = parseInt(call.Args[1])
		}
	}
	return call, true
}

// VFSTrace separates the interleaved stdout of "sqlite3 -vfstrace" into VFS
// calls, ordinary SQL query output lines, and non-call "trace." status lines
// (such as trace.enabled_for("unix")).
type VFSTrace struct {
	Calls  []VFSCall
	Output []string
	Other  []string
}

// ParseVFSTrace parses the stdout stream produced by the sqlite3 CLI with -vfstrace.
func ParseVFSTrace(r io.Reader) (*VFSTrace, error) {
	result := &VFSTrace{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		result.addLine(scanner.Text())
	}
	return result, scanner.Err()
}

func (t *VFSTrace) addLine(line string) {
	if call, ok := ParseVFSLine(line); ok {
		call.Index = len(t.Calls)
		t.Calls = append(t.Calls, call)
		return
	}
	if strings.HasPrefix(line, "trace.") {
		t.Other = append(t.Other, line)
		return
	}
	t.Output = append(t.Output, line)
}

// Methods returns all VFS calls whose Method name is in methods, in original order.
func (t *VFSTrace) Methods(methods ...string) []VFSCall {
	set := map[string]bool{}
	for _, method := range methods {
		set[method] = true
	}
	var out []VFSCall
	for _, call := range t.Calls {
		if set[call.Method] {
			out = append(out, call)
		}
	}
	return out
}

// RelativeVFS strips dir from filenames, arguments, details, and raw lines.
// vfstrace prints base names for open-file methods (xRead, xWrite) and full
// paths for path-based methods (xAccess, xDelete, xFullPathname), so stripping
// dir makes both forms stable across machines.
func RelativeVFS(dir string, calls []VFSCall) []VFSCall {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	out := make([]VFSCall, len(calls))
	for i, call := range calls {
		call.File = strings.ReplaceAll(call.File, prefix, "")
		args := make([]string, len(call.Args))
		for j, arg := range call.Args {
			args[j] = strings.ReplaceAll(arg, prefix, "")
		}
		call.Args = args
		call.Detail = strings.ReplaceAll(call.Detail, prefix, "")
		call.Raw = strings.ReplaceAll(call.Raw, prefix, "")
		out[i] = call
	}
	return out
}

// CLITrace specifies the working directory, database path, and SQL script for
// RunCLIVFSTrace and RunCLIMerged.
type CLITrace struct {
	Dir string
	DB  string
	SQL string
}

func (c CLITrace) args() []string {
	return []string{"-vfstrace"}
}

func (c CLITrace) script() string {
	return sqlitex.OpenScript(c.DB, c.SQL)
}

// RunCLIVFSTrace runs the pinned sqlite3 CLI with -vfstrace and parses its
// stdout (shell.c.in:12911 vfstraceOut writes to p->out, which is stdout at
// shell.c.in:13331). This works on any OS with the pinned CLI installed.
func RunCLIVFSTrace(ctx context.Context, cli sqlitex.CLI, request CLITrace) (*VFSTrace, error) {
	result, err := cli.Run(ctx, sqlitex.CLIRun{Args: request.args(), Stdin: strings.NewReader(request.script()), Dir: request.Dir})
	if err != nil {
		return nil, err
	}
	trace, err := ParseVFSTrace(strings.NewReader(result.Stdout))
	if err != nil {
		return nil, err
	}
	if len(trace.Calls) == 0 {
		return nil, fmt.Errorf("%s -vfstrace produced no VFS calls; is it built with -vfstrace support?", cli.Path)
	}
	return trace, nil
}
