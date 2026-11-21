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

var ErrInjected = errors.New("injected store fault")

type Fault string

const (
	FailBeforePut  Fault = "fail-before-put"
	FailAfterPut   Fault = "fail-after-put"
	PartialPut     Fault = "partial-put"
	CrashBeforePut Fault = "crash-before-put"
	CrashAfterPut  Fault = "crash-after-put"
	StaleList      Fault = "stale-list"
)

type Rule struct {
	Fault Fault
	Match string
	Nth   int
}

func (r Rule) String() string { return fmt.Sprintf("%s:%s:%d", r.Fault, r.Match, r.Nth) }

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

type Store struct {
	Base  walship.Store
	Rules []Rule
	Crash func()
	mu    sync.Mutex
	seen  map[int]int
	fired []string
}

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

func (s *Store) Fired() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.fired...)
}

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

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) { return s.Base.Get(ctx, key) }

func (s *Store) Delete(ctx context.Context, key string) error { return s.Base.Delete(ctx, key) }

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.Base.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return keys, nil
	}
	if fault, ok := s.match(true, prefix); ok && fault == StaleList {
		sort.Strings(keys)
		keys = keys[:len(keys)-1]
	}
	return keys, nil
}
