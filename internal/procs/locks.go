package procs

import (
	"fmt"
	"sort"
	"strings"
)

const (
	PendingByte      int64 = 0x40000000
	ReservedByte           = PendingByte + 1
	SharedFirst            = PendingByte + 2
	SharedSize       int64 = 510
	ShmLockBase      int64 = 120
	ShmLockCount           = 8
	ShmDeadManSwitch       = ShmLockBase + ShmLockCount
)

type LockRange struct {
	Name   string
	Start  int64
	Length int64
}

func (r LockRange) Text() string {
	if r.Length == 1 {
		return fmt.Sprintf("byte %d", r.Start)
	}
	return fmt.Sprintf("bytes %d-%d", r.Start, r.Start+r.Length-1)
}

func DatabaseLockRanges() []LockRange {
	return []LockRange{
		{Name: "PENDING", Start: PendingByte, Length: 1},
		{Name: "RESERVED", Start: ReservedByte, Length: 1},
		{Name: "SHARED", Start: SharedFirst, Length: SharedSize},
	}
}

func ShmLockRanges() []LockRange {
	ranges := []LockRange{
		{Name: "WAL_WRITE_LOCK", Start: ShmLockBase, Length: 1},
		{Name: "WAL_CKPT_LOCK", Start: ShmLockBase + 1, Length: 1},
		{Name: "WAL_RECOVER_LOCK", Start: ShmLockBase + 2, Length: 1},
	}
	for i := 0; i < 5; i++ {
		ranges = append(ranges, LockRange{Name: fmt.Sprintf("WAL_READ_LOCK(%d)", i), Start: ShmLockBase + 3 + int64(i), Length: 1})
	}
	return append(ranges, LockRange{Name: "DMS", Start: ShmDeadManSwitch, Length: 1})
}

type RangeState struct {
	Range  LockRange
	Holder LockHolder
}

func ProbeRanges(path string, ranges []LockRange) ([]RangeState, error) {
	states := make([]RangeState, 0, len(ranges))
	for _, r := range ranges {
		holder, err := ProbeLock(path, r.Start, r.Length)
		if err != nil {
			return nil, err
		}
		states = append(states, RangeState{Range: r, Holder: holder})
	}
	return states, nil
}

func (s RangeState) Text(names func(pid int) string) string {
	switch {
	case !s.Holder.Held:
		return "unlocked"
	case s.Holder.Write:
		return "write-locked by " + names(s.Holder.Pid)
	default:
		return "read-locked"
	}
}

func FormatRangeStates(states []RangeState, names func(pid int) string) []string {
	lines := make([]string, len(states))
	for i, s := range states {
		lines[i] = fmt.Sprintf("%-18s %-16s %s", s.Range.Name, s.Range.Text(), s.Text(names))
	}
	return lines
}

func DescribeProcLocks(locks []ProcLock, names map[int]string, ranges []LockRange) []string {
	lines := make([]string, 0, len(locks))
	for _, lock := range locks {
		label := ""
		for _, r := range ranges {
			if lock.Start == r.Start && !lock.ToEOF && lock.Length() == r.Length {
				label = r.Name
			}
		}
		if label == "" {
			label = "(" + rangeNames(lock, ranges) + ")"
		}
		lines = append(lines, fmt.Sprintf("%-10s %s %-5s %-21s %s", names[lock.Pid], lock.Class, lock.Type, lock.RangeText(), label))
	}
	sort.Strings(lines)
	return lines
}

func rangeNames(lock ProcLock, ranges []LockRange) string {
	var covered []string
	for _, r := range ranges {
		if r.Start <= lock.End && lock.Start <= r.Start+r.Length-1 {
			covered = append(covered, r.Name)
		}
	}
	if len(covered) == 0 {
		return "no named range"
	}
	return strings.Join(covered, "+")
}
