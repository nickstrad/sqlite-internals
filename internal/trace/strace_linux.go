//go:build linux

package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Supported is true on Linux, where strace tracing is available.
const Supported = true

// StracePath locates the strace binary on PATH.
func StracePath() (string, error) {
	path, err := exec.LookPath("strace")
	if err != nil {
		return "", fmt.Errorf("strace not found on PATH: %w", err)
	}
	return path, nil
}

func straceArgs(options Options, output string) ([]string, error) {
	syscalls := options.Syscalls
	if len(syscalls) == 0 {
		syscalls = DefaultSyscalls
	}
	limit := options.StringLimit
	if limit <= 0 {
		limit = 512
	}
	// -f traces all threads/children, -y prints fd paths, -q hides attach/detach noise.
	args := []string{"-f", "-y", "-q", "-s", strconv.Itoa(limit), "-o", output, "-e", "trace=" + strings.Join(syscalls, ",")}
	if options.KillAt != nil {
		if options.KillAt.When < 1 || len(options.KillAt.Syscalls) == 0 {
			return nil, fmt.Errorf("kill point needs at least one syscall and When >= 1, got %+v", *options.KillAt)
		}
		args = append(args, "-e", fmt.Sprintf("inject=%s:signal=SIGKILL:when=%d", strings.Join(options.KillAt.Syscalls, ","), options.KillAt.When))
	}
	return append(append(args, "--"), options.Command...), nil
}

// Run executes options.Command under strace, parses the trace file, and returns
// the result. A non-zero exit or injected SIGKILL of the traced process is
// recorded in Result rather than returned as an error; a ptrace permission
// failure returns an error wrapping ErrPtraceDenied.
func Run(ctx context.Context, options Options) (*Result, error) {
	if len(options.Command) == 0 {
		return nil, errors.New("trace.Run: empty command")
	}
	strace, err := StracePath()
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "course-strace-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	output := filepath.Join(tempDir, "strace.txt")
	args, err := straceArgs(options, output)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, strace, args...)
	cmd.Dir = options.Dir
	cmd.Stdin = options.Stdin
	if options.Env != nil {
		cmd.Env = options.Env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	result := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return result, fmt.Errorf("strace: %w", runErr)
	}
	text, err := os.ReadFile(output)
	if err != nil {
		return result, fmt.Errorf("strace produced no trace (exit %d): %w: %s", result.ExitCode, err, strings.TrimSpace(result.Stderr))
	}
	result.Text = string(text)
	result.Log, err = ParseStraceText(result.Text)
	if err != nil {
		return result, err
	}
	result.Killed = result.Log.KilledBy()
	permissionProblem := strings.Contains(result.Stderr, "ptrace") || strings.Contains(result.Stderr, "Operation not permitted")
	if len(result.Log.Calls) == 0 && permissionProblem {
		return result, fmt.Errorf("%w: %s", ErrPtraceDenied, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

// RunCLIMerged runs the pinned sqlite3 CLI with -vfstrace under strace and
// merges its stdout VFS trace with the underlying syscalls in one ordered stream.
func RunCLIMerged(ctx context.Context, cli sqlitex.CLI, request CLITrace, options Options) (*Merged, *Result, error) {
	options.Command = cli.Command(ctx, sqlitex.CLIRun{Args: request.args()}).Args
	options.Command[0] = cli.Path
	options.Dir = request.Dir
	options.Stdin = strings.NewReader(request.script())
	if len(options.Syscalls) == 0 {
		options.Syscalls = DefaultSyscalls
	}
	// Always trace write and writev so MergeStdoutVFS receives every fd-1 fragment.
	for _, needed := range []string{"write", "writev"} {
		if !containsString(options.Syscalls, needed) {
			options.Syscalls = append(append([]string(nil), options.Syscalls...), needed)
		}
	}
	// Raise the default string limit so long -vfstrace lines on stdout are not truncated.
	if options.StringLimit <= 0 {
		options.StringLimit = 4096
	}
	result, err := Run(ctx, options)
	if err != nil {
		return nil, result, err
	}
	merged, err := MergeStdoutVFS(result.Log, 1, nil)
	if err != nil {
		return nil, result, err
	}
	return merged, result, nil
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
