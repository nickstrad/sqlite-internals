package bounds

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/nickstrad/sqlite-internals/internal/procstat"
)

const (
	DescriptorsPerConnection = 3
	UncountedPerConnection   = 256 * 1024
	MemoryConstant           = 32 << 20
)

var ErrExceeded = errors.New("bounds exceeded")

type Bounds struct {
	OpenDatabases          int
	ConnectionsPerDatabase int
	CacheBytes             int64
	ReserveDescriptors     int
}

func (b Bounds) DescriptorLimit() int {
	return DescriptorsPerConnection*b.ConnectionsPerDatabase*b.OpenDatabases + b.ReserveDescriptors
}

func (b Bounds) MemoryLimit() int64 {
	connections := int64(b.OpenDatabases * b.ConnectionsPerDatabase)
	return connections*(2*b.CacheBytes+UncountedPerConnection) + MemoryConstant
}

func CacheBytes(cacheSize int, pageSize int) int64 {
	if cacheSize < 0 {
		return int64(-cacheSize) * 1024
	}
	return int64(cacheSize) * int64(pageSize)
}

type Sample struct {
	Descriptors  int
	MemoryGrowth int64
}

type Assertion struct {
	Bounds  Bounds
	baseFDs int
	baseRSS int64
	Peak    Sample
	Checks  int
}

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

func (a *Assertion) Summary() string {
	return fmt.Sprintf("standing assertion: %d checks, descriptors above baseline <= %d (%d x %d connections x %d open databases + %d reserve), memory growth <= %d MiB",
		a.Checks, a.Bounds.DescriptorLimit(), DescriptorsPerConnection, a.Bounds.ConnectionsPerDatabase, a.Bounds.OpenDatabases, a.Bounds.ReserveDescriptors, a.Bounds.MemoryLimit()>>20)
}
