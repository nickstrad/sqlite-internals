package faultstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/walship"
)

func base(t *testing.T) *walship.DirStore {
	t.Helper()
	store, err := walship.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPutFaults(t *testing.T) {
	ctx := context.Background()
	data := []byte("0123456789")
	for _, c := range []struct {
		fault  Fault
		stored string
	}{
		{FailBeforePut, ""},
		{FailAfterPut, "0123456789"},
		{PartialPut, "01234"},
	} {
		dir := base(t)
		s := New(dir, Rule{Fault: c.fault, Match: "seg", Nth: 2})
		if err := s.PutIfAbsent(ctx, "seg/a", data); err != nil {
			t.Fatalf("%s: first put: %v", c.fault, err)
		}
		if err := s.PutIfAbsent(ctx, "other/b", data); err != nil {
			t.Fatalf("%s: unmatched put: %v", c.fault, err)
		}
		if err := s.PutIfAbsent(ctx, "seg/b", data); !errors.Is(err, ErrInjected) {
			t.Fatalf("%s: second matching put: %v", c.fault, err)
		}
		got, err := dir.Get(ctx, "seg/b")
		if c.stored == "" && !errors.Is(err, walship.ErrNotFound) || c.stored != "" && string(got) != c.stored {
			t.Fatalf("%s: stored %q %v", c.fault, got, err)
		}
		if err := s.PutIfAbsent(ctx, "seg/c", data); err != nil {
			t.Fatalf("%s: third put: %v", c.fault, err)
		}
		if fired := s.Fired(); len(fired) != 1 || !strings.Contains(fired[0], "seg/b") {
			t.Fatalf("%s: fired %v", c.fault, fired)
		}
	}
}

func TestCrashFaultsCallTheHook(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []Fault{CrashBeforePut, CrashAfterPut} {
		dir := base(t)
		s := New(dir, Rule{Fault: fault, Match: "", Nth: 1})
		crashed := false
		s.Crash = func() { crashed = true }
		if err := s.PutIfAbsent(ctx, "k", []byte("x")); !errors.Is(err, ErrInjected) || !crashed {
			t.Fatalf("%s: %v crashed=%t", fault, err, crashed)
		}
		_, err := dir.Get(ctx, "k")
		if (fault == CrashAfterPut) != (err == nil) {
			t.Fatalf("%s: stored=%t", fault, err == nil)
		}
	}
}

func TestStaleListHidesTheNewestKeyOnce(t *testing.T) {
	ctx := context.Background()
	dir := base(t)
	for _, key := range []string{"log/2", "log/1", "log/3"} {
		if err := dir.PutIfAbsent(ctx, key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	s := New(dir, Rule{Fault: StaleList, Match: "log/", Nth: 1})
	first, err := s.List(ctx, "log/")
	if err != nil || strings.Join(first, ",") != "log/1,log/2" {
		t.Fatalf("%v %v", first, err)
	}
	second, err := s.List(ctx, "log/")
	if err != nil || len(second) != 3 {
		t.Fatalf("%v %v", second, err)
	}
	if err := s.PutIfAbsent(ctx, "log/4", nil); err != nil {
		t.Fatalf("stale-list rule fired on a put: %v", err)
	}
}

func TestParseRule(t *testing.T) {
	rule, err := ParseRule("stale-list:/log/:1")
	if err != nil || rule != (Rule{Fault: StaleList, Match: "/log/", Nth: 1}) || rule.String() != "stale-list:/log/:1" {
		t.Fatalf("%+v %v", rule, err)
	}
	for _, bad := range []string{"", "stale-list", "stale-list:/log/", "stale-list:/log/:0", "stale-list:/log/:x", "explode:/log/:1", "a:b:c:d"} {
		if _, err := ParseRule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
