//go:build linux

package procstat

import (
	"fmt"
	"os"
)

// DescriptorDir is the directory listing open file descriptors for the current
// process on Linux.
const DescriptorDir = "/proc/self/fd"

// TargetsSupported reports whether entries under DescriptorDir are symlinks
// whose file targets can be read with os.Readlink.
const TargetsSupported = true

// MemorySource names the kernel interface ReadMemory reads on Linux.
const MemorySource = "/proc/self/status"

// ReadMemory reads VmRSS and its Anon, File, and Shmem breakdown from
// /proc/self/status.
func ReadMemory() (Memory, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: %w", err)
	}
	return ParseStatus(string(data))
}
