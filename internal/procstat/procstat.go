package procstat

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Descriptor struct {
	FD     int
	Target string
}

type Memory struct {
	RSS   int64
	Anon  int64
	File  int64
	Shmem int64
}

func (m Memory) HasBreakdown() bool {
	return m.Anon >= 0 && m.File >= 0 && m.Shmem >= 0
}

func DescriptorCount() (int, error) {
	fds, err := descriptorNumbers()
	if err != nil {
		return 0, err
	}
	return len(fds), nil
}

func descriptorNumbers() ([]int, error) {
	dir, err := os.Open(DescriptorDir)
	if err != nil {
		return nil, fmt.Errorf("procstat: list descriptors: %w", err)
	}
	defer dir.Close()
	own := int(dir.Fd())
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("procstat: list descriptors: %w", err)
	}
	fds := make([]int, 0, len(names))
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			return nil, fmt.Errorf("procstat: unexpected entry %q in %s", name, DescriptorDir)
		}
		if fd != own {
			fds = append(fds, fd)
		}
	}
	sort.Ints(fds)
	return fds, nil
}

func Descriptors() ([]Descriptor, error) {
	fds, err := descriptorNumbers()
	if err != nil {
		return nil, err
	}
	descriptors := make([]Descriptor, 0, len(fds))
	for _, fd := range fds {
		target := ""
		if TargetsSupported {
			target, err = os.Readlink(filepath.Join(DescriptorDir, strconv.Itoa(fd)))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("procstat: read descriptor %d: %w", fd, err)
			}
		}
		descriptors = append(descriptors, Descriptor{FD: fd, Target: target})
	}
	return descriptors, nil
}

func DescriptorsUnder(dir string) ([]Descriptor, error) {
	if !TargetsSupported {
		return nil, ErrTargetsUnsupported
	}
	prefix, err := directoryPrefix(dir)
	if err != nil {
		return nil, err
	}
	all, err := Descriptors()
	if err != nil {
		return nil, err
	}
	var under []Descriptor
	for _, d := range all {
		if strings.HasPrefix(d.Target, prefix) {
			under = append(under, d)
		}
	}
	return under, nil
}

func directoryPrefix(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("procstat: resolve %s: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if strings.HasSuffix(abs, string(filepath.Separator)) {
		return abs, nil
	}
	return abs + string(filepath.Separator), nil
}

func RelativeTargets(dir string, descriptors []Descriptor) []string {
	prefix, err := directoryPrefix(dir)
	if err != nil {
		prefix = filepath.Clean(dir) + string(filepath.Separator)
	}
	targets := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		targets = append(targets, strings.TrimPrefix(d.Target, prefix))
	}
	sort.Strings(targets)
	return targets
}

var ErrTargetsUnsupported = errors.New("procstat: descriptor targets are only readable from /proc/self/fd on Linux")

func FileLimit() (soft, hard uint64, err error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, 0, fmt.Errorf("procstat: getrlimit: %w", err)
	}
	return uint64(limit.Cur), uint64(limit.Max), nil
}

func parseKiB(value string) (int64, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 || len(fields) > 2 || (len(fields) == 2 && fields[1] != "kB") {
		return 0, fmt.Errorf("procstat: malformed size %q", value)
	}
	return kibToBytes(fields[0])
}

func kibToBytes(text string) (int64, error) {
	kib, err := strconv.ParseInt(text, 10, 64)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("procstat: malformed size %q", text)
	}
	if kib > math.MaxInt64/1024 {
		return 0, fmt.Errorf("procstat: size %s KiB does not fit in int64 bytes", text)
	}
	return kib * 1024, nil
}

func ParseStatus(text string) (Memory, error) {
	m := Memory{RSS: -1, Anon: -1, File: -1, Shmem: -1}
	fields := map[string]*int64{"VmRSS": &m.RSS, "RssAnon": &m.Anon, "RssFile": &m.File, "RssShmem": &m.Shmem}
	for _, line := range strings.Split(text, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		target, wanted := fields[name]
		if !wanted {
			continue
		}
		if *target != -1 {
			return Memory{}, fmt.Errorf("procstat: %s appears twice", name)
		}
		bytes, err := parseKiB(value)
		if err != nil {
			return Memory{}, fmt.Errorf("procstat: %s: %w", name, err)
		}
		*target = bytes
	}
	if m.RSS < 0 {
		return Memory{}, errors.New("procstat: no VmRSS line")
	}
	return m, nil
}

func ParsePSRSS(output string) (Memory, error) {
	text := strings.TrimSpace(output)
	if text == "" || strings.ContainsAny(text, " \t\n") {
		return Memory{}, fmt.Errorf("procstat: malformed ps rss output %q", output)
	}
	bytes, err := kibToBytes(text)
	if err != nil {
		return Memory{}, fmt.Errorf("procstat: ps rss output %q: %w", output, err)
	}
	return Memory{RSS: bytes, Anon: -1, File: -1, Shmem: -1}, nil
}
