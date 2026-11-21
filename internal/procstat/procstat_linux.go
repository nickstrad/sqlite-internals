//go:build linux

package procstat

import (
	"fmt"
	"os"
)

const DescriptorDir = "/proc/self/fd"

const TargetsSupported = true

const MemorySource = "/proc/self/status"

func ReadMemory() (Memory, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: %w", err)
	}
	return ParseStatus(string(data))
}
