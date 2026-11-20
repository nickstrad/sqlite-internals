package trace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

var ErrLinuxOnly = errors.New("strace tracing needs Linux")

func LinuxOnlyMessage(what, packagePath string) string {
	return fmt.Sprintf("LINUX-ONLY: %s skipped on %s; run it with: docker compose run --rm lab go run ./%s", what, runtime.GOOS, packagePath)
}

func WriteLinuxOnly(w io.Writer, what, packagePath string) {
	fmt.Fprintln(w, LinuxOnlyMessage(what, packagePath))
}

func PrintLinuxOnly(what, packagePath string) {
	WriteLinuxOnly(os.Stdout, what, packagePath)
}

type KillAt struct {
	Syscalls []string
	When     int
}

type Options struct {
	Command     []string
	Dir         string
	Env         []string
	Stdin       io.Reader
	Syscalls    []string
	StringLimit int
	KillAt      *KillAt
}

type Result struct {
	Log      *StraceLog
	Stdout   string
	Stderr   string
	ExitCode int
	Killed   string
	Text     string
}

var DefaultSyscalls = []string{
	"openat", "open", "close", "pwrite64", "write", "pread64", "read",
	"fsync", "fdatasync", "unlink", "unlinkat", "fcntl", "ftruncate", "mmap", "munmap",
}

func SelfCommand(args ...string) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return append([]string{self}, args...), nil
}
