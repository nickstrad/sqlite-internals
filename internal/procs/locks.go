package procs

import (
	"fmt"
	"sort"
	"strings"
)

// POSIX byte-range lock offsets defined by SQLite at version-3.53.4:
// database file lock bytes in src/os.h:160-166 and -shm lock bytes in
// src/os_unix.c:4602-4603 and src/wal.c:294-299.
const (
	// PendingByte is the 1 GiB boundary byte locked exclusively during PENDING (0x40000000).
	PendingByte int64 = 0x40000000
	// ReservedByte is the byte locked exclusively during RESERVED (0x40000001).
	ReservedByte = PendingByte + 1
	// SharedFirst is the first byte of the 510-byte SHARED/EXCLUSIVE range (0x40000002).
	SharedFirst = PendingByte + 2
	// SharedSize is the length of the SHARED lock range (510 bytes, src/os.h:166).
	SharedSize int64 = 510
	// ShmLockBase is the first WAL lock byte in -shm: (22+8)*4 = 120 (src/os_unix.c:4602).
	ShmLockBase int64 = 120
	// ShmLockCount is SQLITE_SHM_NLOCK (8 slots: WRITE, CKPT, RECOVER, READ 0..4).
	ShmLockCount = 8
	// ShmDeadManSwitch is byte 128 of -shm, held shared by every WAL connection (src/os_unix.c:4603).
	ShmDeadManSwitch = ShmLockBase + ShmLockCount
)

// LockRange names one POSIX byte range that SQLite locks on a database or -shm file.
type LockRange struct {
	Name   string
	Start  int64
	Length int64
}

// Text formats r as "byte N" for single-byte locks or "bytes A-B" for multi-byte ranges.
func (r LockRange) Text() string {
	if r.Length == 1 {
		return fmt.Sprintf("byte %d", r.Start)
	}
	return fmt.Sprintf("bytes %d-%d", r.Start, r.Start+r.Length-1)
}

// DatabaseLockRanges returns the three POSIX lock byte ranges on the main
// database file (PENDING, RESERVED, SHARED; src/os.h:160-166).
func DatabaseLockRanges() []LockRange {
	return []LockRange{
		{Name: "PENDING", Start: PendingByte, Length: 1},
		{Name: "RESERVED", Start: ReservedByte, Length: 1},
		{Name: "SHARED", Start: SharedFirst, Length: SharedSize},
	}
}

// ShmLockRanges returns the nine POSIX lock bytes on the WAL -shm file:
// WAL_WRITE_LOCK (120), WAL_CKPT_LOCK (121), WAL_RECOVER_LOCK (122),
// WAL_READ_LOCK(0..4) (123-127), and DMS (128) (src/wal.c:294-299, src/os_unix.c:4602-4603).
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

// RangeState pairs a named LockRange with the LockHolder returned by fcntl(F_GETLK).
type RangeState struct {
	Range  LockRange
	Holder LockHolder
}

// ProbeRanges calls ProbeLock for each range in ranges on path.
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

// Text formats the probed lock state. Because fcntl(F_GETLK) reports only one
// arbitrary holder when multiple processes share a read lock, only write locks
// (which have a unique holder) print the holder's logical name.
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

// FormatRangeStates aligns each RangeState into a readable table row.
func FormatRangeStates(states []RangeState, names func(pid int) string) []string {
	lines := make([]string, len(states))
	for i, s := range states {
		lines[i] = fmt.Sprintf("%-18s %-16s %s", s.Range.Name, s.Range.Text(), s.Text(names))
	}
	return lines
}

// DescribeProcLocks formats filtered /proc/locks entries with child names and
// SQLite range labels, sorted lexicographically for deterministic evidence.
// A kernel lock merged across adjacent named bytes is labelled "(A+B)".
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
