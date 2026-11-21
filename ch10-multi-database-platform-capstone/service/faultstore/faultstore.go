// Package faultstore wraps any walship.Store to inject one-shot faults around
// puts and listings while keeping the underlying real store (SeaweedFS S3 in
// experiments, DirStore in unit tests) in the loop.
//
// Each Rule matches calls whose key or list prefix contains Match and fires
// once on the Nth matching call (1-based). Put faults simulate dropped,
// ambiguous, partial, or crashing uploads; stale-list simulates delayed list
// visibility by hiding the lexicographically last key once.
package faultstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickstrad/sqlite-internals/internal/walship"
)

// ErrInjected is returned whenever a fault rule fires on PutIfAbsent.
var ErrInjected = errors.New("injected store fault")

// Fault names a failure mode that Store can inject on a matching call.
type Fault string

// Supported Fault values.
const (
	// FailBeforePut returns ErrInjected without calling the underlying store.
	FailBeforePut Fault = "fail-before-put"
	// FailAfterPut stores the full object and then returns ErrInjected,
	// modeling an ambiguous upload whose reply was lost.
	FailAfterPut Fault = "fail-after-put"
	// PartialPut stores only the first half of the bytes and returns
	// ErrInjected, stressing readers against non-atomic uploads.
	PartialPut Fault = "partial-put"
	// CrashBeforePut calls Store.Crash before calling the underlying store.
	CrashBeforePut Fault = "crash-before-put"
	// CrashAfterPut calls Store.Crash after the underlying PutIfAbsent succeeds.
	CrashAfterPut Fault = "crash-after-put"
	// StaleList omits the lexicographically last key from one non-empty List.
	StaleList Fault = "stale-list"
)

// Rule specifies one fault to fire on the Nth (1-based) call whose key or list
// prefix contains Match.
type Rule struct {
	Fault Fault
	Match string
	Nth   int
}

// String formats r in the "fault:match:nth" syntax accepted by ParseRule.
func (r Rule) String() string { return fmt.Sprintf("%s:%s:%d", r.Fault, r.Match, r.Nth) }

// ParseRule parses a "fault:match:nth" specification, requiring exactly three
// colon-separated fields, a known Fault name, and a positive integer nth.
func ParseRule(text string) (Rule, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 3 {
		return Rule{}, fmt.Errorf("fault rule %q: want fault:match:nth", text)
	}
	nth, err := strconv.Atoi(parts[2])
	if err != nil || nth < 1 {
		return Rule{}, fmt.Errorf("fault rule %q: nth must be a positive integer", text)
	}
	rule := Rule{Fault: Fault(parts[0]), Match: parts[1], Nth: nth}
	switch rule.Fault {
	case FailBeforePut, FailAfterPut, PartialPut, CrashBeforePut, CrashAfterPut, StaleList:
		return rule, nil
	}
	return Rule{}, fmt.Errorf("fault rule %q: unknown fault %q", text, parts[0])
}

// Store is a walship.Store wrapper that forwards operations to Base and fires
// matching Rules once. Crash defaults to os.Exit(3) and can be replaced in tests.
type Store struct {
	Base  walship.Store
	Rules []Rule
	Crash func()
	mu    sync.Mutex
	seen  map[int]int
	fired []string
}

// New wraps base with rules and installs the default os.Exit(3) Crash hook.
func New(base walship.Store, rules ...Rule) *Store {
	return &Store{Base: base, Rules: rules, Crash: func() { os.Exit(3) }, seen: map[int]int{}}
}

func (s *Store) match(isList bool, key string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rule := range s.Rules {
		if (rule.Fault == StaleList) != isList || !strings.Contains(key, rule.Match) {
			continue
		}
		s.seen[i]++
		if s.seen[i] == rule.Nth {
			s.fired = append(s.fired, fmt.Sprintf("%s on %s", rule.Fault, key))
			return rule.Fault, true
		}
	}
	return "", false
}

// Fired returns a copy of the log of rules that have fired so far.
func (s *Store) Fired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.fired...)
}

// PutIfAbsent forwards to Base unless a put fault rule matches key on its Nth
// occurrence, in which case it executes that fault mode and returns ErrInjected.
func (s *Store) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	fault, ok := s.match(false, key)
	if !ok {
		return s.Base.PutIfAbsent(ctx, key, data)
	}
	switch fault {
	case FailBeforePut:
		return fmt.Errorf("%w: %s before put of %s", ErrInjected, fault, key)
	case CrashBeforePut:
		s.Crash()
		return fmt.Errorf("%w: %s before put of %s", ErrInjected, fault, key)
	case PartialPut:
		if err := s.Base.PutIfAbsent(ctx, key, data[:len(data)/2]); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s stored %d of %d bytes of %s", ErrInjected, fault, len(data)/2, len(data), key)
	}
	if err := s.Base.PutIfAbsent(ctx, key, data); err != nil {
		return err
	}
	if fault == CrashAfterPut {
		s.Crash()
	}
	return fmt.Errorf("%w: %s after put of %s", ErrInjected, fault, key)
}

// Get forwards directly to Base.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) { return s.Base.Get(ctx, key) }

// Delete forwards directly to Base.
func (s *Store) Delete(ctx context.Context, key string) error { return s.Base.Delete(ctx, key) }

// List forwards to Base and, if a StaleList rule fires on a non-empty result,
// sorts the keys and drops the last (newest fixed-width) key once.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.Base.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	// An empty listing has nothing to hide, so do not consume a StaleList
	// rule's Nth count or record it as fired.
	if len(keys) == 0 {
		return keys, nil
	}
	if fault, ok := s.match(true, prefix); ok && fault == StaleList {
		sort.Strings(keys)
		keys = keys[:len(keys)-1]
	}
	return keys, nil
}
