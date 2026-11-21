package model

import (
	"strings"
	"testing"
)

func TestCorrectModelHasNoViolation(t *testing.T) {
	report := Explore(Correct, 10, 1, 200, 40)
	if report.Violating != 0 || report.WalkViolating != 0 || report.Shortest != nil {
		t.Fatalf("correct model violated an invariant: %+v", report)
	}
	if report.Interleavings < 1_000_000 || report.States < 1000 || report.Walks != 200 {
		t.Fatalf("explored too little: %+v", report)
	}
}

func TestMutantsAreCaught(t *testing.T) {
	for _, m := range []Mutant{StoreAcceptsEveryPut, ResumeFromGlobalEnd} {
		report := Explore(m, 10, 1, 0, 0)
		if report.Violating == 0 || len(report.Shortest) == 0 || report.Violation == "" {
			t.Fatalf("%s: no violation found: %+v", m, report)
		}
	}
}

func TestCountsDoNotDependOnSeed(t *testing.T) {
	a, b := Explore(Correct, 9, 1, 0, 0), Explore(Correct, 9, 77, 0, 0)
	if a.Interleavings != b.Interleavings || a.States != b.States {
		t.Fatalf("seed changed the explored set: %d/%d vs %d/%d", a.Interleavings, a.States, b.Interleavings, b.States)
	}
}

func run(t *testing.T, mutant Mutant, script string) ([]string, State) {
	t.Helper()
	actions, err := ParseScript(script)
	if err != nil {
		t.Fatal(err)
	}
	return Replay(mutant, actions)
}

func TestStaleOwnerIsRefusedAfterTheFence(t *testing.T) {
	lines, s := run(t, Correct, "A claim, A fence, A write, A ship, B claim, B fence, A write, A ship, A write")
	if !strings.Contains(lines[7], "refused: position 3 holds epoch 2") || !strings.Contains(lines[8], "not enabled (fenced)") {
		t.Fatalf("stale owner not refused:\n%s", strings.Join(lines, "\n"))
	}
	if got := strings.Join(s.ChainLines(), "; "); got != "positions 1-1 epoch 1 snapshot; positions 2-2 epoch 1 segment; positions 3-3 epoch 2 snapshot" {
		t.Fatalf("chain %s", got)
	}
	if v := s.Violation(); v != "" {
		t.Fatal(v)
	}
}

func TestStaleListingRetriesThenFencesAfterTheEnd(t *testing.T) {
	lines, s := run(t, Correct, "A claim, A fence, A write, A ship, B claim, B fence-with-stale-list, B fence")
	if !strings.HasSuffix(lines[5], "retry") || !strings.HasSuffix(lines[6], "ok: fence at 3") || s.Nodes[1].Role != Owner {
		t.Fatalf("%s", strings.Join(lines, "\n"))
	}
}

func TestRestartedStaleOwnerIsFenced(t *testing.T) {
	_, s := run(t, Correct, "A claim, A fence, B claim, B fence, A crash, A restart")
	if s.Nodes[0].Role != Fenced || s.Nodes[0].Belief != 0 {
		t.Fatalf("restarted node %+v", s.Nodes[0])
	}
	_, s = run(t, ResumeFromGlobalEnd, "A claim, A fence, B claim, B fence, A crash, A restart, A ship")
	if !strings.Contains(s.Violation(), "after epoch 2") {
		t.Fatalf("mutant violation %q", s.Violation())
	}
}

func TestViolationDetectsEachInvariant(t *testing.T) {
	var s State
	s.Accepted[0], s.Accepted[1], s.NAccepted = Object{Epoch: 1, First: 1, Last: 2}, Object{Epoch: 2, First: 2, Last: 2}, 2
	s.Slots[1], s.Slots[2] = s.Accepted[0], s.Accepted[1]
	if !strings.Contains(s.Violation(), "both accepted") {
		t.Fatal(s.Violation())
	}
	s.Accepted[0], s.Accepted[1] = Object{Epoch: 2, First: 1, Last: 1}, Object{Epoch: 1, First: 2, Last: 2}
	s.Slots[1], s.Slots[2] = s.Accepted[0], s.Accepted[1]
	if !strings.Contains(s.Violation(), "after epoch 2") {
		t.Fatal(s.Violation())
	}
	s.NAccepted = 1
	s.Slots[1] = Object{Epoch: 3, First: 1, Last: 1}
	if !strings.Contains(s.Violation(), "overwritten") {
		t.Fatal(s.Violation())
	}
}

func TestParseScriptRejectsBadActions(t *testing.T) {
	for _, bad := range []string{"", "A", "C claim", "A jump", "AA claim", "A claim extra"} {
		if _, err := ParseScript(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseMutant(t *testing.T) {
	for _, m := range []Mutant{Correct, StoreAcceptsEveryPut, ResumeFromGlobalEnd} {
		if got, err := ParseMutant(string(m)); got != m || err != nil {
			t.Errorf("%s: %q %v", m, got, err)
		}
	}
	for _, name := range []string{"", "bogus-mutant", "Correct"} {
		if _, err := ParseMutant(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}
