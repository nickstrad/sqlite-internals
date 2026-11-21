//go:build !linux

package procstat

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

const DescriptorDir = "/dev/fd"

const TargetsSupported = false

const MemorySource = "ps -o rss= -p <pid>"

func ReadMemory() (Memory, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: ps: %w", err)
	}
	return ParsePSRSS(string(out))
}
