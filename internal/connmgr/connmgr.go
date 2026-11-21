// Package connmgr provides a bounded LRU connection manager for services that
// hold many SQLite database files on one machine. At most Capacity databases
// stay open at once; on eviction each closed database is verified at rest and
// its SHA-256 recorded so unexpected changes while closed are caught on reopen.
package connmgr

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

var (
	// ErrClosed is returned when Acquire or Close is called on a closed Manager.
	ErrClosed = errors.New("connmgr: manager is closed")
	// ErrInUse is returned by Evict or Close when at least one Handle has not been released.
	ErrInUse = errors.New("connmgr: database is in use")
	// ErrNotOpen is returned by Evict when the requested database ID is not currently open.
	ErrNotOpen = errors.New("connmgr: database is not open")
)

// ChangedWhileClosedError is returned by Acquire when a previously evicted
// database is no longer at rest or its size or SHA-256 differs from the
// Status recorded when the manager closed it.
type ChangedWhileClosedError struct {
	ID       string
	Path     string
	Recorded Status
	Found    Status
}

// Error formats the mismatch between the recorded at-rest digest and what
// CheckAtRest observed before reopening.
func (e *ChangedWhileClosedError) Error() string {
	if !e.Found.AtRest {
		return fmt.Sprintf("connmgr: %s changed while closed: recorded at rest with sha256 %s, now %s", e.ID, e.Recorded.SHA256, e.Found)
	}
	return fmt.Sprintf("connmgr: %s changed while closed: recorded sha256 %s, file now has sha256 %s", e.ID, e.Recorded.SHA256, e.Found.SHA256)
}

// Cause identifies why a database connection was evicted from a Manager.
type Cause string

const (
	// CauseCapacity means an idle database was closed to free a slot for Acquire.
	CauseCapacity Cause = "capacity"
	// CauseExplicit means the caller invoked Manager.Evict.
	CauseExplicit Cause = "explicit"
	// CauseShutdown means Manager.Close closed every remaining database.
	CauseShutdown Cause = "shutdown"
)

// Eviction reports the outcome of closing one managed database and checking
// whether it landed at rest.
type Eviction struct {
	ID        string
	Path      string
	Cause     Cause
	CloseErr  error
	Status    Status
	StatusErr error
}

// Options configures a Manager. Capacity (>= 1), PathFor, and Open are required.
type Options struct {
	Capacity int
	PathFor  func(id string) (string, error)
	Open     func(ctx context.Context, path string) (*sqlitex.DB, error)
	OnEvict  func(Eviction)
}

// Stats holds cumulative counters and current occupancy for a Manager.
// Misses always equals VerifiedReopens + UnverifiedOpens.
type Stats struct {
	Open              int
	MaxOpen           int
	Hits              int
	Misses            int
	Evictions         int
	EvictedAtRest     int
	EvictedNotAtRest  int
	VerifiedReopens   int
	UnverifiedOpens   int
	RefusedReopens    int
	CapacityWaits     int
	OpenFailures      int
	CloseFailures     int
	StatusFailures    int
	DistinctDatabases int
}

type entry struct {
	id   string
	path string
	db   *sqlitex.DB
	refs int
	elem *list.Element
}

// Manager keeps at most Options.Capacity SQLite databases open at once,
// evicting the least recently used idle database when a new ID is acquired.
// One mutex serializes open, close, and hash checks.
type Manager struct {
	options  Options
	mu       sync.Mutex
	open     map[string]*entry
	lru      *list.List
	records  map[string]Status
	seen     map[string]bool
	stats    Stats
	released chan struct{}
	closed   bool
}

// Handle is a lease on one open database managed by a Manager. Callers must
// close all *sql.Rows, *sql.Tx, and *sql.Conn derived from DB before calling
// Release so a subsequent eviction can actually close the SQLite connection.
type Handle struct {
	ID       string
	Path     string
	DB       *sqlitex.DB
	manager  *Manager
	entry    *entry
	released bool
}

// New validates options and returns a ready Manager with zero open databases.
func New(options Options) (*Manager, error) {
	switch {
	case options.Capacity < 1:
		return nil, fmt.Errorf("connmgr: capacity %d, need at least 1", options.Capacity)
	case options.PathFor == nil:
		return nil, errors.New("connmgr: PathFor is required")
	case options.Open == nil:
		return nil, errors.New("connmgr: Open is required")
	}
	return &Manager{
		options:  options,
		open:     make(map[string]*entry),
		lru:      list.New(),
		records:  make(map[string]Status),
		seen:     make(map[string]bool),
		released: make(chan struct{}),
	}, nil
}

// Acquire returns a Handle for id, reusing an already-open connection when
// present. On a miss it evicts the least recently used idle database if at
// capacity (or waits until a Handle is released or ctx expires), verifies any
// recorded at-rest hash, and opens the database.
func (m *Manager) Acquire(ctx context.Context, id string) (*Handle, error) {
	var evictions []Eviction
	// Invoke OnEvict after releasing m.mu so hooks may call Manager methods.
	defer func() { m.report(evictions) }()
	m.mu.Lock()
	waited := false
	for {
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		if e, ok := m.open[id]; ok {
			e.refs++
			m.lru.MoveToFront(e.elem)
			m.stats.Hits++
			m.mu.Unlock()
			return m.handle(e), nil
		}
		// Reject a cancelled or expired context before evicting a healthy idle
		// database that would not be replaced.
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if len(m.open) < m.options.Capacity {
			break
		}
		if victim := m.leastRecentlyUsedIdle(); victim != nil {
			evictions = append(evictions, m.evictLocked(victim, CauseCapacity))
			continue
		}
		if !waited {
			m.stats.CapacityWaits++
			waited = true
		}
		wake := m.released
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	path, err := m.options.PathFor(id)
	if err != nil {
		m.stats.OpenFailures++
		return nil, fmt.Errorf("connmgr: path for %s: %w", id, err)
	}
	verified, err := m.verifyBeforeOpen(id, path)
	if err != nil {
		return nil, err
	}
	db, err := m.options.Open(ctx, path)
	if err != nil {
		m.stats.OpenFailures++
		return nil, fmt.Errorf("connmgr: open %s: %w", id, err)
	}
	// Update miss and open-kind counters only after Open succeeds so
	// Misses == VerifiedReopens + UnverifiedOpens holds across Open failures.
	m.stats.Misses++
	if verified {
		m.stats.VerifiedReopens++
	} else {
		m.stats.UnverifiedOpens++
	}
	if !m.seen[id] {
		m.seen[id] = true
		m.stats.DistinctDatabases++
	}
	e := &entry{id: id, path: path, db: db, refs: 1}
	e.elem = m.lru.PushFront(e)
	m.open[id] = e
	m.stats.Open = len(m.open)
	m.stats.MaxOpen = max(m.stats.MaxOpen, m.stats.Open)
	return m.handle(e), nil
}

func (m *Manager) handle(e *entry) *Handle {
	return &Handle{ID: e.id, Path: e.path, DB: e.db, manager: m, entry: e}
}

func (m *Manager) verifyBeforeOpen(id, path string) (bool, error) {
	recorded, ok := m.records[id]
	if !ok {
		return false, nil
	}
	found, err := CheckAtRest(path)
	if err != nil {
		m.stats.StatusFailures++
		return false, fmt.Errorf("connmgr: check %s before reopening: %w", id, err)
	}
	if !found.AtRest || found.SHA256 != recorded.SHA256 || found.Size != recorded.Size {
		m.stats.RefusedReopens++
		return false, &ChangedWhileClosedError{ID: id, Path: path, Recorded: recorded, Found: found}
	}
	return true, nil
}

// Release returns the Handle to the Manager, allowing the database to be
// evicted once its reference count reaches zero and waking any waiting Acquire
// calls. Calling Release more than once is a no-op.
func (h *Handle) Release() {
	m := h.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	h.entry.refs--
	close(m.released)
	m.released = make(chan struct{})
}

func (m *Manager) leastRecentlyUsedIdle() *entry {
	for elem := m.lru.Back(); elem != nil; elem = elem.Prev() {
		if e := elem.Value.(*entry); e.refs == 0 {
			return e
		}
	}
	return nil
}

func (m *Manager) evictLocked(e *entry, cause Cause) Eviction {
	m.lru.Remove(e.elem)
	delete(m.open, e.id)
	m.stats.Open = len(m.open)
	m.stats.Evictions++
	eviction := Eviction{ID: e.id, Path: e.path, Cause: cause}
	if err := e.db.Close(); err != nil {
		eviction.CloseErr = err
		m.stats.CloseFailures++
	}
	// Skip CheckAtRest whenever Close failed or database/sql still holds a
	// checked-out connection (unclosed *sql.Rows, *sql.Tx, or *sql.Conn):
	// (*sql.DB).Close closes only idle connections and returns nil, while
	// CheckAtRest's open+close would drop the live connection's POSIX locks.
	switch stillOpen := e.db.Stats().OpenConnections; {
	case eviction.CloseErr != nil:
		eviction.Status = Status{Path: e.path, Problems: []string{fmt.Sprintf("close failed: %v", eviction.CloseErr)}}
	case stillOpen > 0:
		eviction.Status = Status{Path: e.path, Problems: []string{fmt.Sprintf("%d connection(s) still open in this process: a statement, transaction or *sql.Conn was not closed, so database/sql closes the connection only when it is returned", stillOpen)}}
	default:
		eviction.Status, eviction.StatusErr = CheckAtRest(e.path)
	}
	switch {
	case eviction.StatusErr != nil:
		m.stats.StatusFailures++
		delete(m.records, e.id)
	case eviction.Status.AtRest:
		m.stats.EvictedAtRest++
		m.records[e.id] = eviction.Status
	default:
		m.stats.EvictedNotAtRest++
		delete(m.records, e.id)
	}
	return eviction
}

func (m *Manager) report(evictions []Eviction) {
	if m.options.OnEvict == nil {
		return
	}
	for _, eviction := range evictions {
		m.options.OnEvict(eviction)
	}
}

// Evict closes the idle open database identified by id, checks whether it is
// at rest, records its digest if so, and runs OnEvict. It returns ErrNotOpen
// if id is not open or ErrInUse if a Handle is still held.
func (m *Manager) Evict(id string) (Eviction, error) {
	m.mu.Lock()
	e, ok := m.open[id]
	if !ok {
		m.mu.Unlock()
		return Eviction{}, ErrNotOpen
	}
	if e.refs > 0 {
		m.mu.Unlock()
		return Eviction{}, ErrInUse
	}
	eviction := m.evictLocked(e, CauseExplicit)
	m.mu.Unlock()
	m.report([]Eviction{eviction})
	return eviction, nil
}

// Close evicts every open database and marks the Manager closed. It returns
// ErrInUse without closing anything if any Handle is still outstanding, and
// ErrClosed if called a second time.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	var busy []string
	for elem := m.lru.Front(); elem != nil; elem = elem.Next() {
		if e := elem.Value.(*entry); e.refs > 0 {
			busy = append(busy, e.id)
		}
	}
	if len(busy) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("%w: %d handle(s) not released: %v", ErrInUse, len(busy), busy)
	}
	var evictions []Eviction
	for m.lru.Len() > 0 {
		evictions = append(evictions, m.evictLocked(m.lru.Back().Value.(*entry), CauseShutdown))
	}
	m.closed = true
	close(m.released)
	m.mu.Unlock()
	m.report(evictions)
	var errs []error
	for _, eviction := range evictions {
		if eviction.CloseErr != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", eviction.ID, eviction.CloseErr))
		}
		if eviction.StatusErr != nil {
			errs = append(errs, fmt.Errorf("check %s: %w", eviction.ID, eviction.StatusErr))
		}
	}
	return errors.Join(errs...)
}

// Stats returns a snapshot of the Manager's current counters.
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// Record returns the at-rest Status recorded when id was last evicted cleanly.
func (m *Manager) Record(id string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, ok := m.records[id]
	return status, ok
}

// OpenIDs returns the IDs of currently open databases from most recently used
// to least recently used.
func (m *Manager) OpenIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, m.lru.Len())
	for elem := m.lru.Front(); elem != nil; elem = elem.Next() {
		ids = append(ids, elem.Value.(*entry).id)
	}
	return ids
}

// Forget drops the stored at-rest record for id so a deliberate external change
// (such as a restore) can be opened without triggering ChangedWhileClosedError.
func (m *Manager) Forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.records[id]
	delete(m.records, id)
	return ok
}
