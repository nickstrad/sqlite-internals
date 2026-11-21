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
	ErrClosed  = errors.New("connmgr: manager is closed")
	ErrInUse   = errors.New("connmgr: database is in use")
	ErrNotOpen = errors.New("connmgr: database is not open")
)

type ChangedWhileClosedError struct {
	ID       string
	Path     string
	Recorded Status
	Found    Status
}

func (e *ChangedWhileClosedError) Error() string {
	if !e.Found.AtRest {
		return fmt.Sprintf("connmgr: %s changed while closed: recorded at rest with sha256 %s, now %s", e.ID, e.Recorded.SHA256, e.Found)
	}
	return fmt.Sprintf("connmgr: %s changed while closed: recorded sha256 %s, file now has sha256 %s", e.ID, e.Recorded.SHA256, e.Found.SHA256)
}

type Cause string

const (
	CauseCapacity Cause = "capacity"
	CauseExplicit Cause = "explicit"
	CauseShutdown Cause = "shutdown"
)

type Eviction struct {
	ID        string
	Path      string
	Cause     Cause
	CloseErr  error
	Status    Status
	StatusErr error
}

type Options struct {
	Capacity int
	PathFor  func(id string) (string, error)
	Open     func(ctx context.Context, path string) (*sqlitex.DB, error)
	OnEvict  func(Eviction)
}

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

type Handle struct {
	ID       string
	Path     string
	DB       *sqlitex.DB
	manager  *Manager
	entry    *entry
	released bool
}

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

func (m *Manager) Acquire(ctx context.Context, id string) (*Handle, error) {
	var evictions []Eviction
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

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

func (m *Manager) Record(id string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, ok := m.records[id]
	return status, ok
}

func (m *Manager) OpenIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, m.lru.Len())
	for elem := m.lru.Front(); elem != nil; elem = elem.Next() {
		ids = append(ids, elem.Value.(*entry).id)
	}
	return ids
}

func (m *Manager) Forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.records[id]
	delete(m.records, id)
	return ok
}
