package trace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

// ErrLinuxOnly is returned by strace runners on non-Linux operating systems.
var ErrLinuxOnly = errors.New("strace tracing needs Linux")

// ErrPtraceDenied is wrapped by Run when strace fails due to missing ptrace permissions.
var ErrPtraceDenied = errors.New("strace could not trace (needs ptrace; in Docker add cap SYS_PTRACE)")

// LinuxOnlyMessage formats the standard skip line printed by strace-based
// experiments when run outside Linux. cmd/course check evidence recognizes the
// "LINUX-ONLY:" prefix on non-Linux hosts.
func LinuxOnlyMessage(what, packagePath string) string {
	return fmt.Sprintf("LINUX-ONLY: %s skipped on %s; run it with: docker compose run --rm lab go run ./%s", what, runtime.GOOS, packagePath)
}

// WriteLinuxOnly writes LinuxOnlyMessage followed by a newline to w.
func WriteLinuxOnly(w io.Writer, what, packagePath string) {
	fmt.Fprintln(w, LinuxOnlyMessage(what, packagePath))
}

// PrintLinuxOnly writes LinuxOnlyMessage to stdout.
func PrintLinuxOnly(what, packagePath string) {
	WriteLinuxOnly(os.Stdout, what, packagePath)
}

// KillAt configures strace's fault injection (-e inject=...:signal=SIGKILL:when=K)
// to kill the traced thread on entry to its When-th call matching Syscalls.
// Because strace counts per thread, a Go child process should call
// runtime.LockOSThread before running SQLite statements.
type KillAt struct {
	Syscalls []string
	When     int
}

// Options configures one strace invocation via Run or RunCLIMerged.
type Options struct {
	Command     []string
	Dir         string
	Env         []string
	Stdin       io.Reader
	Syscalls    []string
	StringLimit int
	KillAt      *KillAt
}

// Result holds the parsed trace, captured output, and exit status from Run.
// When strace kills the tracee with an injected SIGKILL, strace re-raises the
// signal on itself, so ExitCode is -1 and Killed is "SIGKILL".
type Result struct {
	Log      *StraceLog
	Stdout   string
	Stderr   string
	ExitCode int
	Killed   string
	Text     string
}

// DefaultSyscalls lists the file, lock, sync, and mapping syscalls traced when
// Options.Syscalls is empty. Both open and openat are included because
// modernc.org/sqlite uses open on linux/amd64 while C builds use openat; both
// fsync and fdatasync are included because default ./configure builds of the
// pinned tree call fdatasync while modernc calls fsync.
var DefaultSyscalls = []string{
	"openat", "open", "close", "pwrite64", "write", "writev", "pread64", "read",
	"fsync", "fdatasync", "unlink", "unlinkat", "fcntl", "ftruncate", "mmap", "munmap",
}

// SelfCommand returns the path to the currently running executable followed by
// args so an experiment can re-run itself as a traced child process under go run.
func SelfCommand(args ...string) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return append([]string{self}, args...), nil
}
