package procs

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const ProcLocksPath = "/proc/locks"

const NoInodeField = "<none>:0"

var ErrProcLocksFormat = errors.New("procs: malformed /proc/locks line")

type ProcLock struct {
	ID       int64
	Blocked  bool
	Class    string
	Mode     string
	Type     string
	Pid      int
	DevMajor uint32
	DevMinor uint32
	Inode    uint64
	Start    int64
	End      int64
	ToEOF    bool
}

func (l ProcLock) Length() int64 {
	if l.ToEOF {
		return math.MaxInt64
	}
	return l.End - l.Start + 1
}

func (l ProcLock) RangeText() string {
	if l.ToEOF {
		return fmt.Sprintf("%d-EOF", l.Start)
	}
	if l.Start == l.End {
		return strconv.FormatInt(l.Start, 10)
	}
	return fmt.Sprintf("%d-%d", l.Start, l.End)
}

func ParseProcLocks(text string) ([]ProcLock, error) {
	var locks []ProcLock
	scanner := bufio.NewScanner(strings.NewReader(text))
	number := 0
	for scanner.Scan() {
		number++
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		lock, err := ParseProcLockLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number, err)
		}
		locks = append(locks, lock)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return locks, nil
}

func ParseProcLockLine(line string) (ProcLock, error) {
	bad := func(why string) (ProcLock, error) {
		return ProcLock{}, fmt.Errorf("%w (%s): %q", ErrProcLocksFormat, why, line)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || !strings.HasSuffix(fields[0], ":") {
		return bad("no id")
	}
	var lock ProcLock
	id, err := strconv.ParseInt(strings.TrimSuffix(fields[0], ":"), 10, 64)
	if err != nil || id <= 0 {
		return bad("id")
	}
	lock.ID = id
	rest := fields[1:]
	if len(rest) > 0 && rest[0] == "->" {
		lock.Blocked = true
		rest = rest[1:]
	}
	if len(rest) != 7 {
		return bad(fmt.Sprintf("want 7 fields after the id, got %d", len(rest)))
	}
	switch rest[0] {
	case "POSIX", "OFDLCK", "FLOCK", "ACCESS", "LEASE", "DELEG", "UNKNOWN":
		lock.Class = rest[0]
	default:
		return bad("class")
	}
	switch rest[1] {
	case "ADVISORY", "MANDATORY", "ACTIVE", "BREAKING", "BREAKER", "*NOINODE*", "MSNFS", "UNKNOWN":
		lock.Mode = rest[1]
	default:
		return bad("mode")
	}
	switch rest[2] {
	case "READ", "WRITE", "UNLCK", "RW", "NONE":
		lock.Type = rest[2]
	default:
		return bad("type")
	}
	pid, err := strconv.Atoi(rest[3])
	if err != nil {
		return bad("pid")
	}
	lock.Pid = pid
	if rest[4] == NoInodeField {
		return parseLockRange(lock, rest[5], rest[6], bad)
	}
	device := strings.Split(rest[4], ":")
	if len(device) != 3 {
		return bad("device and inode")
	}
	major, err := strconv.ParseUint(device[0], 16, 32)
	if err != nil {
		return bad("device major")
	}
	minor, err := strconv.ParseUint(device[1], 16, 32)
	if err != nil {
		return bad("device minor")
	}
	inode, err := strconv.ParseUint(device[2], 10, 64)
	if err != nil {
		return bad("inode")
	}
	lock.DevMajor, lock.DevMinor, lock.Inode = uint32(major), uint32(minor), inode
	return parseLockRange(lock, rest[5], rest[6], bad)
}

func parseLockRange(lock ProcLock, startField, endField string, bad func(string) (ProcLock, error)) (ProcLock, error) {
	start, err := strconv.ParseInt(startField, 10, 64)
	if err != nil || start < 0 {
		return bad("start")
	}
	lock.Start = start
	if endField == "EOF" {
		lock.ToEOF = true
		lock.End = math.MaxInt64
		return lock, nil
	}
	end, err := strconv.ParseInt(endField, 10, 64)
	if err != nil || end < start {
		return bad("end")
	}
	lock.End = end
	return lock, nil
}

func FilterProcLocks(locks []ProcLock, inode uint64, pids map[int]bool) []ProcLock {
	var out []ProcLock
	for _, lock := range locks {
		if lock.Inode == inode && pids[lock.Pid] {
			out = append(out, lock)
		}
	}
	return out
}
