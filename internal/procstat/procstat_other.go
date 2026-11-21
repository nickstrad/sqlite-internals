//go:build !linux

package procstat

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// DescriptorDir is the directory listing open file descriptors on non-Linux
// Unix platforms (such as macOS fdesc).
const DescriptorDir = "/dev/fd"

// TargetsSupported is false off Linux because /dev/fd entries are not symlinks
// pointing to file paths.
const TargetsSupported = false

// MemorySource names the external command ReadMemory runs off Linux.
const MemorySource = "ps -o rss= -p <pid>"

// ReadMemory queries the current process's resident set size via ps.
func ReadMemory() (Memory, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: ps: %w", err)
	}
	return ParsePSRSS(string(out))
}
