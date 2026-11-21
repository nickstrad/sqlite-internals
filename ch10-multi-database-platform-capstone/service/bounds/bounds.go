// Package bounds owns the standing resource-bound assertion run inside every
// capstone node process after every command.
//
// It reuses the per-connection descriptor and memory formulas established in
// Chapter 8 (Experiment 8.3). A service-held database keeps 3 connections open
// (the writer plus walship's control and pin connections), which actually open
// 7 descriptors per database (3 .db + 3 -wal + 1 process-shared -shm through
// unixInodeInfo, src/os_unix.c:1323-1342); DescriptorLimit uses 3 descriptors
// per connection as a conservative upper bound.
package bounds

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/nickstrad/walfleet/internal/procstat"
)

// Per-connection upper-bound constants carried forward from Chapters 4 and 8.
const (
	// DescriptorsPerConnection is the per-connection WAL descriptor upper bound
	// measured in Experiment 8.2.
	DescriptorsPerConnection = 3
	// UncountedPerConnection is the per-connection schema/statement/overhead
	// allowance from Experiment 8.2.
	UncountedPerConnection = 256 * 1024
	// MemoryConstant is the fixed Go-runtime and process-baseline margin from
	// Experiment 8.3.
	MemoryConstant = 32 << 20
)

// ErrExceeded is returned by Assertion.Check when either open descriptors or
// RSS growth above the startup baseline exceeds Bounds.
var ErrExceeded = errors.New("bounds exceeded")

// Bounds holds the capacity parameters used to derive descriptor and RSS limits.
type Bounds struct {
	OpenDatabases          int
	ConnectionsPerDatabase int
	CacheBytes             int64
	ReserveDescriptors     int
}

// DescriptorLimit returns the maximum descriptor growth above baseline:
// DescriptorsPerConnection * ConnectionsPerDatabase * OpenDatabases + ReserveDescriptors.
func (b Bounds) DescriptorLimit() int {
	return DescriptorsPerConnection*b.ConnectionsPerDatabase*b.OpenDatabases + b.ReserveDescriptors
}

// MemoryLimit returns the maximum RSS growth above baseline in bytes:
// totalConnections * (2*CacheBytes + UncountedPerConnection) + MemoryConstant,
// where the factor of 2 covers SQLite's page-cache allocator overhead (Chapter 4).
func (b Bounds) MemoryLimit() int64 {
	connections := int64(b.OpenDatabases * b.ConnectionsPerDatabase)
	return connections*(2*b.CacheBytes+UncountedPerConnection) + MemoryConstant
}

// CacheBytes converts SQLite's PRAGMA cache_size value to bytes: negative
// values specify kibibytes, non-negative values specify page counts.
func CacheBytes(cacheSize int, pageSize int) int64 {
	if cacheSize < 0 {
		return int64(-cacheSize) * 1024
	}
	return int64(cacheSize) * int64(pageSize)
}

// Sample holds one measurement of open descriptors and RSS growth above baseline.
type Sample struct {
	Descriptors  int
	MemoryGrowth int64
}

// Assertion tracks a process's descriptor and RSS baseline, peak samples, and
// check count against Bounds.
type Assertion struct {
	Bounds  Bounds
	baseFDs int
	baseRSS int64
	Peak    Sample
	Checks  int
}

// Start records the current descriptor count and GC-settled RSS as the
// baseline for subsequent Check calls.
func Start(b Bounds) (*Assertion, error) {
	fds, err := procstat.DescriptorCount()
	if err != nil {
		return nil, err
	}
	rss, err := settledRSS()
	if err != nil {
		return nil, err
	}
	return &Assertion{Bounds: b, baseFDs: fds, baseRSS: rss}, nil
}

func settledRSS() (int64, error) {
	runtime.GC()
	debug.FreeOSMemory()
	memory, err := procstat.ReadMemory()
	return memory.RSS, err
}

// Check samples current descriptors and RSS relative to the baseline, updates
// Peak and Checks, and returns ErrExceeded if either limit is crossed.
func (a *Assertion) Check() (Sample, error) {
	fds, err := procstat.DescriptorCount()
	if err != nil {
		return Sample{}, err
	}
	memory, err := procstat.ReadMemory()
	if err != nil {
		return Sample{}, err
	}
	sample := Sample{Descriptors: fds - a.baseFDs, MemoryGrowth: memory.RSS - a.baseRSS}
	a.Checks++
	a.Peak.Descriptors = max(a.Peak.Descriptors, sample.Descriptors)
	a.Peak.MemoryGrowth = max(a.Peak.MemoryGrowth, sample.MemoryGrowth)
	if sample.Descriptors > a.Bounds.DescriptorLimit() || sample.MemoryGrowth > a.Bounds.MemoryLimit() {
		return sample, fmt.Errorf("%w: %d descriptors above the baseline (limit %d), memory growth %d bytes (limit %d)",
			ErrExceeded, sample.Descriptors, a.Bounds.DescriptorLimit(), sample.MemoryGrowth, a.Bounds.MemoryLimit())
	}
	return sample, nil
}

// Summary formats a stable one-line description of a's check count and limits
// for experiment output.
func (a *Assertion) Summary() string {
	return fmt.Sprintf("standing assertion: %d checks, descriptors above baseline <= %d (%d x %d connections x %d open databases + %d reserve), memory growth <= %d MiB",
		a.Checks, a.Bounds.DescriptorLimit(), DescriptorsPerConnection, a.Bounds.ConnectionsPerDatabase, a.Bounds.OpenDatabases, a.Bounds.ReserveDescriptors, a.Bounds.MemoryLimit()>>20)
}
