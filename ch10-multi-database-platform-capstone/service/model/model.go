package model

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

type Mutant string

const (
	Correct              Mutant = "correct"
	StoreAcceptsEveryPut Mutant = "store-accepts-every-put"
	ResumeFromGlobalEnd  Mutant = "resume-from-global-end"
)

func ParseMutant(name string) (Mutant, error) {
	switch m := Mutant(name); m {
	case Correct, StoreAcceptsEveryPut, ResumeFromGlobalEnd:
		return m, nil
	}
	return "", fmt.Errorf("model: unknown mutant %q (want %s, %s or %s)", name, Correct, StoreAcceptsEveryPut, ResumeFromGlobalEnd)
}

const (
	Nodes       = 2
	MaxLocal    = 5
	MaxEpoch    = 3
	slotCount   = MaxLocal + MaxEpoch + 2
	maxAccepted = 16
)

type Kind uint8

const (
	Claim Kind = iota
	Fence
	FenceStale
	Write
	Ship
	Crash
	Restart
	RestartStale
	kindCount
)

var kindNames = [kindCount]string{"claim", "fence", "fence-with-stale-list", "write", "ship", "crash", "restart", "restart-with-stale-list"}

func (k Kind) String() string { return kindNames[k] }

type Action struct {
	Node uint8
	Kind Kind
}

func (a Action) String() string { return fmt.Sprintf("%c %s", 'A'+a.Node, a.Kind) }

type Role uint8

const (
	Idle Role = iota
	Claiming
	Owner
	Fenced
	Stuck
)

type Object struct {
	Epoch    uint8
	First    uint8
	Last     uint8
	Snapshot bool
}

type NodeState struct {
	Up      bool
	Role    Role
	Epoch   uint8
	Belief  uint8
	Local   uint8
	Shipped uint8
}

type State struct {
	CatalogEpoch uint8
	Slots        [slotCount]Object
	Nodes        [Nodes]NodeState
	Accepted     [maxAccepted]Object
	NAccepted    uint8
}

func Initial() State {
	var s State
	for i := range s.Nodes {
		s.Nodes[i].Up = true
	}
	return s
}

func (s *State) chain(stale bool) []Object {
	var objects []Object
	for p := 1; p < slotCount && s.Slots[p].Epoch != 0; p = int(s.Slots[p].Last) + 1 {
		objects = append(objects, s.Slots[p])
		if s.Slots[p].Last < s.Slots[p].First {
			break
		}
	}
	if stale && len(objects) > 0 {
		objects = objects[:len(objects)-1]
	}
	return objects
}

func end(objects []Object) uint8 {
	if len(objects) == 0 {
		return 0
	}
	return objects[len(objects)-1].Last
}

func newerThan(objects []Object, epoch uint8) bool {
	for _, o := range objects {
		if o.Epoch > epoch {
			return true
		}
	}
	return false
}

func (s *State) put(mutant Mutant, o Object) (accepted, inBounds bool) {
	if int(o.Last) >= slotCount || s.NAccepted == maxAccepted {
		return false, false
	}
	if s.Slots[o.First].Epoch != 0 && mutant != StoreAcceptsEveryPut {
		return false, true
	}
	s.Slots[o.First] = o
	s.Accepted[s.NAccepted] = o
	s.NAccepted++
	return true, true
}

func Step(s State, mutant Mutant, a Action) (State, string, bool) {
	n := &s.Nodes[a.Node]
	switch a.Kind {
	case Claim:
		if !n.Up || n.Role == Claiming || n.Role == Owner || s.CatalogEpoch == MaxEpoch {
			return s, "", false
		}
		s.CatalogEpoch++
		n.Epoch, n.Role = s.CatalogEpoch, Claiming
		return s, fmt.Sprintf("ok: catalog epoch %d", n.Epoch), true
	case Fence, FenceStale:
		if !n.Up || n.Role != Claiming || (a.Kind == FenceStale && s.Slots[1].Epoch == 0) {
			return s, "", false
		}
		view := s.chain(a.Kind == FenceStale)
		if newerThan(view, n.Epoch) {
			n.Role = Idle
			return s, "refused: a newer epoch is in the store", true
		}
		at := end(view) + 1
		accepted, inBounds := s.put(mutant, Object{Epoch: n.Epoch, First: at, Last: at, Snapshot: true})
		switch {
		case !inBounds:
			return s, "", false
		case accepted:
			n.Role, n.Belief, n.Local, n.Shipped = Owner, n.Epoch, at, at
			return s, fmt.Sprintf("ok: fence at %d", at), true
		case s.Slots[at].Epoch > n.Epoch:
			n.Role = Idle
			return s, fmt.Sprintf("refused: position %d holds epoch %d", at, s.Slots[at].Epoch), true
		}
		return s, fmt.Sprintf("refused: position %d holds epoch %d, retry", at, s.Slots[at].Epoch), true
	case Write:
		if !n.Up || n.Role != Owner || n.Local >= MaxLocal {
			return s, "", false
		}
		n.Local++
		return s, fmt.Sprintf("ok: local commit %d", n.Local), true
	case Ship:
		if !n.Up || n.Role != Owner || n.Local <= n.Shipped {
			return s, "", false
		}
		first := n.Shipped + 1
		accepted, inBounds := s.put(mutant, Object{Epoch: n.Epoch, First: first, Last: n.Local})
		switch {
		case !inBounds:
			return s, "", false
		case accepted:
			n.Shipped = n.Local
			return s, fmt.Sprintf("ok: positions %d-%d accepted", first, n.Local), true
		case s.Slots[first].Epoch > n.Epoch:
			n.Role, n.Belief = Fenced, 0
			return s, fmt.Sprintf("refused: position %d holds epoch %d", first, s.Slots[first].Epoch), true
		}
		n.Role = Stuck
		return s, fmt.Sprintf("refused: position %d holds epoch %d", first, s.Slots[first].Epoch), true
	case Crash:
		if !n.Up {
			return s, "", false
		}
		n.Up, n.Role, n.Epoch, n.Shipped = false, Idle, 0, 0
		return s, "ok: volatile state lost", true
	case Restart, RestartStale:
		if n.Up || (a.Kind == RestartStale && s.Slots[1].Epoch == 0) {
			return s, "", false
		}
		n.Up = true
		if n.Belief == 0 {
			return s, "ok: no ownership belief", true
		}
		n.Epoch, n.Role = n.Belief, Owner
		view := s.chain(a.Kind == RestartStale)
		if mutant == ResumeFromGlobalEnd {
			n.Shipped = end(view)
			n.Local = n.Shipped + 1
			return s, fmt.Sprintf("ok: resumed at the store's end %d", n.Shipped), true
		}
		if newerThan(view, n.Epoch) {
			n.Role, n.Belief = Fenced, 0
			return s, "ok: restarted, then refused: a newer epoch is in the store", true
		}
		n.Shipped = end(view)
		if n.Local < n.Shipped {
			n.Local = n.Shipped
		}
		return s, fmt.Sprintf("ok: resumed own chain at %d", n.Shipped), true
	}
	return s, "", false
}

func overlaps(a, b Object) bool { return a.First <= b.Last && b.First <= a.Last }

func (s *State) Violation() string {
	highest := uint8(0)
	for i := 0; i < int(s.NAccepted); i++ {
		a := s.Accepted[i]
		for j := 0; j < i; j++ {
			if b := s.Accepted[j]; b.Epoch != a.Epoch && overlaps(a, b) {
				return fmt.Sprintf("epochs %d and %d were both accepted for positions %d-%d and %d-%d", b.Epoch, a.Epoch, b.First, b.Last, a.First, a.Last)
			}
		}
		if a.Epoch < highest {
			return fmt.Sprintf("epoch %d was accepted at positions %d-%d after epoch %d had been accepted", a.Epoch, a.First, a.Last, highest)
		}
		highest = max(highest, a.Epoch)
		if s.Slots[a.First] != a {
			return fmt.Sprintf("the accepted object of epoch %d at position %d was overwritten", a.Epoch, a.First)
		}
	}
	return ""
}

func actions() []Action {
	var all []Action
	for node := uint8(0); node < Nodes; node++ {
		for kind := Kind(0); kind < kindCount; kind++ {
			all = append(all, Action{Node: node, Kind: kind})
		}
	}
	return all
}

type Report struct {
	Mutant        Mutant
	Depth         int
	Interleavings uint64
	Violating     uint64
	States        int
	Shortest      []string
	Violation     string
	Walks         int
	WalkSteps     int
	WalkViolating int
}

type memoKey struct {
	state State
	depth int
}

type counts struct{ paths, violating uint64 }

func Explore(mutant Mutant, depth int, seed uint64, walks, walkLength int) Report {
	order := actions()
	rand.New(rand.NewPCG(seed, 1001)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	report := Report{Mutant: mutant, Depth: depth}
	memo := map[memoKey]counts{}
	var count func(State, int) counts
	count = func(s State, d int) counts {
		if s.Violation() != "" {
			return counts{1, 1}
		}
		if d == 0 {
			return counts{1, 0}
		}
		key := memoKey{s, d}
		if c, ok := memo[key]; ok {
			return c
		}
		var total counts
		for _, a := range order {
			if next, _, ok := Step(s, mutant, a); ok {
				c := count(next, d-1)
				total.paths += c.paths
				total.violating += c.violating
			}
		}
		if total.paths == 0 {
			total = counts{1, 0}
		}
		memo[key] = total
		return total
	}
	c := count(Initial(), depth)
	report.Interleavings, report.Violating = c.paths, c.violating
	report.States, report.Shortest, report.Violation = breadthFirst(mutant, depth, order)
	rng := rand.New(rand.NewPCG(seed, 2002))
	for w := 0; w < walks; w++ {
		s := Initial()
		for step := 0; step < walkLength; step++ {
			var enabled []State
			for _, a := range order {
				if next, _, ok := Step(s, mutant, a); ok {
					enabled = append(enabled, next)
				}
			}
			if len(enabled) == 0 {
				break
			}
			s = enabled[rng.IntN(len(enabled))]
			report.WalkSteps++
			if s.Violation() != "" {
				report.WalkViolating++
				break
			}
		}
		report.Walks++
	}
	return report
}

type visit struct {
	parent State
	action Action
	depth  int
}

func breadthFirst(mutant Mutant, depth int, order []Action) (int, []string, string) {
	start := Initial()
	seen := map[State]visit{start: {depth: 0}}
	frontier := []State{start}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []State
		for _, s := range frontier {
			for _, a := range order {
				t, _, ok := Step(s, mutant, a)
				if !ok {
					continue
				}
				if _, done := seen[t]; done {
					continue
				}
				seen[t] = visit{parent: s, action: a, depth: d + 1}
				if v := t.Violation(); v != "" {
					return len(seen), trace(mutant, seen, t), v
				}
				next = append(next, t)
			}
		}
		frontier = next
	}
	return len(seen), nil, ""
}

func trace(mutant Mutant, seen map[State]visit, last State) []string {
	var path []Action
	for s := last; seen[s].depth > 0; s = seen[s].parent {
		path = append([]Action{seen[s].action}, path...)
	}
	lines, _ := Replay(mutant, path)
	return lines
}

func Replay(mutant Mutant, script []Action) ([]string, State) {
	s := Initial()
	var lines []string
	for i, a := range script {
		next, outcome, ok := Step(s, mutant, a)
		if !ok {
			outcome = "refused: not enabled (" + roleName(s.Nodes[a.Node]) + ")"
		} else {
			s = next
		}
		lines = append(lines, fmt.Sprintf("%2d. %-28s %s", i+1, a.String(), outcome))
	}
	return lines, s
}

func roleName(n NodeState) string {
	if !n.Up {
		return "down"
	}
	return [...]string{"idle", "claiming", "owner", "fenced", "stuck"}[n.Role]
}

func (s State) ChainLines() []string {
	var lines []string
	for _, o := range s.chain(false) {
		kind := "segment"
		if o.Snapshot {
			kind = "snapshot"
		}
		lines = append(lines, fmt.Sprintf("positions %d-%d epoch %d %s", o.First, o.Last, o.Epoch, kind))
	}
	return lines
}

func ParseScript(text string) ([]Action, error) {
	var script []Action
	for _, item := range strings.Split(text, ",") {
		fields := strings.Fields(item)
		if len(fields) != 2 || len(fields[0]) != 1 || fields[0][0] < 'A' || fields[0][0] >= 'A'+Nodes {
			return nil, fmt.Errorf("model: bad action %q", item)
		}
		kind := Kind(0)
		for kind < kindCount && kindNames[kind] != fields[1] {
			kind++
		}
		if kind == kindCount {
			return nil, fmt.Errorf("model: unknown action kind %q", fields[1])
		}
		script = append(script, Action{Node: fields[0][0] - 'A', Kind: kind})
	}
	return script, nil
}
