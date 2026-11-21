// Command model-and-mustfail attacks Chapter 10a's ownership state-machine
// claims and runs both model-level and real-store "must be able to fail" checks.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/model"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const (
	s3Bucket = "course-ch10a"
	s3Prefix = "adv-ch10a-mod"
)

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func scratchDir(root, name string) string {
	dir := filepath.Join(root, "_work", "adv-ch10a", "model-and-mustfail", name)
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch "+name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch "+name, err)
	}
	return dir
}

// overwritingS3Store strips If-None-Match from PutIfAbsent on real SeaweedFS S3
// so MOD-02 can test whether removing slot exclusivity actually breaks one-writer
// on real SQLite + real S3 and whether fence.Audit detects it.
type overwritingS3Store struct {
	*s3store.Store
}

func (s *overwritingS3Store) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := walship.ValidateKey(key); err != nil {
		return err
	}
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.Prefix + key),
		Body:   bytes.NewReader(data),
	})
	return err
}

func attackMOD01(root string) {
	// Build 10.1-one-writer once into scratch so subprocess checks run fast.
	bin := filepath.Join(scratchDir(root, "mod01"), "exp10-1")
	build := exec.Command("go", "build", "-o", bin, "./ch10-multi-database-platform-capstone/experiments/10.1-one-writer")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fatal("build 10.1-one-writer", fmt.Errorf("%v: %s", err, out))
	}

	type runCase struct {
		mutant      string
		expectExit0 bool
		expectSub   string
	}
	cases := []runCase{
		{"correct", true, "all checks passed"},
		{"store-accepts-every-put", false, "check FAILED: model store-accepts-every-put"},
		{"resume-from-global-end", false, "check FAILED: model resume-from-global-end"},
	}

	var details []string
	held := true
	for _, tc := range cases {
		cmd := exec.Command(bin, "-model-only", "-mutant", tc.mutant)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		text := string(out)
		exitOK := err == nil
		hasSub := strings.Contains(text, tc.expectSub)
		if exitOK != tc.expectExit0 || !hasSub {
			held = false
		}
		details = append(details, fmt.Sprintf("%s(exit0=%t,matched=%t)", tc.mutant, exitOK, hasSub))
	}
	report(
		"MOD-01",
		"run 10.1-one-writer -model-only with correct and both mutants",
		"correct exits 0; both mutants fail main check and exit non-zero",
		strings.Join(details, "; "),
		held,
	)
}

func attackMOD02(ctx context.Context, root string, base *s3store.Store) {
	// Must-be-able-to-fail on real SQLite + real SeaweedFS S3: strip If-None-Match:*
	// so slot 3 is silently overwritten by stale epoch 1 after epoch 2 fenced at slot 3.
	if _, err := base.Wipe(ctx); err != nil {
		fatal("wipe s3 for mod02", err)
	}
	work := scratchDir(root, "mod02")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("mod02 catalog", err)
	}
	defer cat.Close()
	if err := cat.Register(ctx, "db-1", node.PlatformSettings()); err != nil {
		fatal("mod02 register", err)
	}

	brokenStore := &overwritingS3Store{Store: base}
	workload := recipe.Default()
	newNode := func(name string) *node.Node {
		n, err := node.Start(node.Config{
			Name:     name,
			Dir:      filepath.Join(work, name),
			Catalog:  cat,
			Store:    brokenStore,
			Capacity: 2,
			File:     workload.SQLiteOptions().File,
			Create:   func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
		})
		if err != nil {
			fatal("mod02 start "+name, err)
		}
		return n
	}
	applyNext := func(ctx context.Context, db *sqlitex.DB) error {
		_, err := workload.ApplyNext(ctx, db)
		return err
	}

	a := newNode("a")
	b := newNode("b")
	defer a.Close()
	defer b.Close()

	if _, err := a.Claim(ctx, "db-1"); err != nil {
		fatal("mod02 a claim", err)
	}
	if _, err := a.Write(ctx, "db-1", node.StoreDurable, applyNext); err != nil {
		fatal("mod02 a write 1", err)
	}
	// b claims epoch 2 and places its fence snapshot at slot 3, then opens db-1
	// (via Read) so its shipper caches slot 3 as the end of its chain.
	claimB, err := b.Claim(ctx, "db-1")
	if err != nil {
		fatal("mod02 b claim", err)
	}
	if err := b.Read(ctx, "db-1", func(ctx context.Context, db *sqlitex.DB) error {
		var uv int
		return db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uv)
	}); err != nil {
		fatal("mod02 b open via read", err)
	}
	// Stale node a writes store-durable without knowing b fenced slot 3.
	staleAck, staleErr := a.Write(ctx, "db-1", node.StoreDurable, applyNext)
	// b also writes store-durable at slot 4 (thinking slot 3 holds its epoch 2 fence).
	bAck, bErr := b.Write(ctx, "db-1", node.StoreDurable, applyNext)

	// Audit the resulting real S3 chain using fence.Audit.
	chain, _, auditErr := fence.Audit(ctx, base, "db-1")
	var chainDesc []string
	for _, s := range chain {
		chainDesc = append(chainDesc, fmt.Sprintf("p%d-%d:e%d:%s", s.First, s.Last, s.Epoch, s.Kind))
	}

	// Without If-None-Match:*, stale node a's write at slot 3 succeeds and overwrites
	// b's epoch 2 fence snapshot; b's slot 4 then starts epoch 2 with a segment instead
	// of a fence snapshot, which fence.Audit must catch as ErrBadChain.
	staleSucceeded := staleErr == nil && staleAck.Position == 3 && staleAck.Epoch == 1
	bSucceeded := bErr == nil && bAck.Position == 4 && bAck.Epoch == 2
	auditCaught := errors.Is(auditErr, fence.ErrBadChain)
	held := claimB.Fence == 3 && staleSucceeded && bSucceeded && auditCaught
	report(
		"MOD-02",
		"real S3 must-fail: remove If-None-Match:* on SeaweedFS and run stale-owner takeover",
		"stale epoch-1 write overwrites epoch-2 fence at slot 3 and fence.Audit flags ErrBadChain",
		fmt.Sprintf("staleAck=(pos=%d,e=%d,err=%v) bAck=(pos=%d,e=%d,err=%v) chain=[%s] auditErr=%v",
			staleAck.Position, staleAck.Epoch, staleErr, bAck.Position, bAck.Epoch, bErr, strings.Join(chainDesc, ","), auditErr),
		held,
	)
}

func attackMOD03(ctx context.Context, root string) {
	// Real-store must-be-able-to-fail #2: point raw walship.Open (without fence.Adapter)
	// at a shared store after a new owner took over and appended position 3.
	// Confirm raw walship resumes from the global end and appends stale state at position 4
	// even though PutIfAbsent is strictly enforced by DirStore.
	work := scratchDir(root, "mod03")
	store, err := walship.NewDirStore(filepath.Join(work, "store"))
	if err != nil {
		fatal("mod03 store", err)
	}
	workload := recipe.Default()
	opts := workload.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = 0

	// Node A creates DB (step 0), opens raw shipper (snapshot pos 1), commits step 1 (segment pos 2).
	pathA := filepath.Join(work, "a.db")
	if err := workload.Build(ctx, pathA, 0); err != nil {
		fatal("mod03 build a", err)
	}
	shipA, err := walship.Open(ctx, walship.Config{Database: pathA, Store: store, Options: opts, Origin: 1})
	if err != nil {
		fatal("mod03 open shipA", err)
	}
	dbA, err := workload.Open(ctx, pathA)
	if err != nil {
		fatal("mod03 open dbA", err)
	}
	if err := workload.ApplyStep(ctx, dbA, 1); err != nil {
		fatal("mod03 step 1 on a", err)
	}
	if _, err := shipA.Cycle(ctx); err != nil {
		fatal("mod03 cycle a", err)
	}
	// A commits local step 2 (unshipped) and "crashes" (close handles without shipping).
	if err := workload.ApplyStep(ctx, dbA, 2); err != nil {
		fatal("mod03 local step 2 on a", err)
	}
	_ = dbA.Close()
	_ = shipA.Close()

	// Node B takes over by restoring position 2 (step 1) into b.db and shipping a new step (pos 3).
	catBefore, err := walship.LoadCatalog(ctx, store)
	if err != nil {
		fatal("mod03 load catalog for b", err)
	}
	plan2, err := catBefore.Plan(2)
	if err != nil {
		fatal("mod03 plan 2", err)
	}
	img2, _, err := plan2.Execute()
	if err != nil {
		fatal("mod03 exec plan 2", err)
	}
	pathB := filepath.Join(work, "b.db")
	if err := walship.WriteDatabase(pathB, img2); err != nil {
		fatal("mod03 write b", err)
	}
	shipB, err := walship.Open(ctx, walship.Config{Database: pathB, Store: store, Options: opts})
	if err != nil {
		fatal("mod03 open shipB", err)
	}
	if _, err := shipB.AdoptAtRest(ctx); err != nil {
		fatal("mod03 adopt b", err)
	}
	dbB, err := workload.Open(ctx, pathB)
	if err != nil {
		fatal("mod03 open dbB", err)
	}
	// B commits a distinct single transaction (user_version 99) at position 3.
	if _, err := dbB.ExecContext(ctx, "BEGIN IMMEDIATE; INSERT INTO accounts (name, balance) VALUES ('node-b-branch', 999); PRAGMA user_version = 99; COMMIT;"); err != nil {
		fatal("mod03 write on b", err)
	}
	repB, err := shipB.Cycle(ctx)
	if err != nil {
		fatal("mod03 cycle b", err)
	}
	_ = dbB.Close()
	_ = shipB.Close()

	// Now restarted stale Node A opens raw walship.Open WITHOUT fence.Adapter.
	shipA2, err := walship.Open(ctx, walship.Config{Database: pathA, Store: store, Options: opts})
	if err != nil {
		fatal("mod03 reopen shipA", err)
	}
	resumedPos := shipA2.Position()
	repA2, cycleErr := shipA2.Cycle(ctx)
	_ = shipA2.Close()

	// Raw walship resumed at B's position (3) and published A's stale local file (user_version 2,
	// erasing B's user_version 99 commit) at position 4 despite PutIfAbsent!
	corruptedHistory := cycleErr == nil && repB.Segment != nil && resumedPos == 3 &&
		repA2.Snapshot != nil && repA2.Snapshot.Position == 4 && repA2.Snapshot.UserVersion == 2
	report(
		"MOD-03",
		"real-store must-fail: raw walship.Open without fence.Adapter on restarted stale owner",
		"raw walship resumes at global end (pos 3) and appends stale local state (uv=2) at pos 4, clobbering B's uv=99",
		fmt.Sprintf("resumedPos=%d cycleErr=%v snap=%+v", resumedPos, cycleErr, repA2.Snapshot),
		corruptedHistory,
	)
}

// Independent 3-node, 5-epoch, arbitrary-lag stale-listing model of the fencing protocol.
// Tests beyond 10.1's fixed bounds (2 nodes, 3 epochs, 1-slot stale lag).
const (
	indNodes    = 3
	indMaxEpoch = 5
	indMaxPos   = 12
	indMaxAcc   = 20
)

type indObj struct {
	epoch uint8
	first uint8
	last  uint8
	snap  bool
}

type indNode struct {
	up      bool
	role    uint8 // 0=idle, 1=claiming, 2=owner, 3=fenced, 4=stuck
	epoch   uint8
	belief  uint8
	local   uint8
	shipped uint8
}

type indState struct {
	catEpoch  uint8
	slots     [indMaxPos + 2]indObj
	nodes     [indNodes]indNode
	accepted  [indMaxAcc]indObj
	nAccepted uint8
}

func indInit() indState {
	var s indState
	for i := range s.nodes {
		s.nodes[i].up = true
	}
	return s
}

func (s *indState) chain(hideTail int) []indObj {
	var out []indObj
	for p := 1; p < len(s.slots) && s.slots[p].epoch != 0; p = int(s.slots[p].last) + 1 {
		out = append(out, s.slots[p])
		if s.slots[p].last < s.slots[p].first {
			break
		}
	}
	if hideTail > 0 && len(out) > 0 {
		if hideTail >= len(out) {
			return nil
		}
		out = out[:len(out)-hideTail]
	}
	return out
}

func (s *indState) violation() string {
	highest := uint8(0)
	for i := 0; i < int(s.nAccepted); i++ {
		a := s.accepted[i]
		for j := 0; j < i; j++ {
			b := s.accepted[j]
			if b.epoch != a.epoch && a.first <= b.last && b.first <= a.last {
				return fmt.Sprintf("overlap e%d[%d-%d] vs e%d[%d-%d]", b.epoch, b.first, b.last, a.epoch, a.first, a.last)
			}
		}
		if a.epoch < highest {
			return fmt.Sprintf("stale epoch %d accepted after epoch %d", a.epoch, highest)
		}
		if a.epoch > highest {
			highest = a.epoch
		}
		if s.slots[a.first] != a {
			return fmt.Sprintf("slot %d overwritten", a.first)
		}
	}
	return ""
}

func indStep(s indState, nodeIdx uint8, op uint8, lag int, allowOverwrite bool) (indState, bool) {
	// ops: 0=claim, 1=fence(lag), 2=write, 3=ship, 4=crash, 5=restart(lag)
	n := &s.nodes[nodeIdx]
	switch op {
	case 0: // claim
		if !n.up || n.role == 1 || n.role == 2 || s.catEpoch >= indMaxEpoch {
			return s, false
		}
		s.catEpoch++
		n.epoch, n.role = s.catEpoch, 1
		return s, true
	case 1: // fence with arbitrary lag (0, 1, 2, or full hide)
		if !n.up || n.role != 1 {
			return s, false
		}
		view := s.chain(lag)
		for _, o := range view {
			if o.epoch > n.epoch {
				n.role = 0
				return s, true
			}
		}
		at := uint8(1)
		if len(view) > 0 {
			at = view[len(view)-1].last + 1
		}
		if int(at) >= len(s.slots) || s.nAccepted >= indMaxAcc {
			return s, false
		}
		obj := indObj{epoch: n.epoch, first: at, last: at, snap: true}
		if s.slots[at].epoch == 0 || allowOverwrite {
			s.slots[at] = obj
			s.accepted[s.nAccepted] = obj
			s.nAccepted++
			n.role, n.belief, n.local, n.shipped = 2, n.epoch, at, at
			return s, true
		}
		if s.slots[at].epoch > n.epoch {
			n.role = 0
			return s, true
		}
		return s, true // retry
	case 2: // write
		if !n.up || n.role != 2 || n.local >= indMaxPos {
			return s, false
		}
		n.local++
		return s, true
	case 3: // ship
		if !n.up || n.role != 2 || n.local <= n.shipped {
			return s, false
		}
		first := n.shipped + 1
		if int(n.local) >= len(s.slots) || s.nAccepted >= indMaxAcc {
			return s, false
		}
		obj := indObj{epoch: n.epoch, first: first, last: n.local, snap: false}
		if s.slots[first].epoch == 0 || allowOverwrite {
			s.slots[first] = obj
			s.accepted[s.nAccepted] = obj
			s.nAccepted++
			n.shipped = n.local
			return s, true
		}
		if s.slots[first].epoch > n.epoch {
			n.role, n.belief = 3, 0
			return s, true
		}
		n.role = 4
		return s, true
	case 4: // crash
		if !n.up {
			return s, false
		}
		n.up, n.role, n.epoch, n.shipped = false, 0, 0, 0
		return s, true
	case 5: // restart with arbitrary lag
		if n.up {
			return s, false
		}
		n.up = true
		if n.belief == 0 {
			return s, true
		}
		n.epoch, n.role = n.belief, 2
		view := s.chain(lag)
		for _, o := range view {
			if o.epoch > n.epoch {
				n.role, n.belief = 3, 0
				return s, true
			}
		}
		end := uint8(0)
		if len(view) > 0 {
			end = view[len(view)-1].last
		}
		n.shipped = end
		if n.local < n.shipped {
			n.local = n.shipped
		}
		return s, true
	}
	return s, false
}

func attackMOD04() {
	// Independent 3-node / 5-epoch / multi-slot-stale-list checker:
	// 50,000 random walks of 80 steps + BFS depth 6, plus mutant control.
	type move struct {
		node uint8
		op   uint8
		lag  int
	}
	var allMoves []move
	for nodeIdx := uint8(0); nodeIdx < indNodes; nodeIdx++ {
		allMoves = append(allMoves,
			move{nodeIdx, 0, 0},
			move{nodeIdx, 1, 0},
			move{nodeIdx, 1, 1},
			move{nodeIdx, 1, 2},
			move{nodeIdx, 1, 99}, // completely empty stale listing
			move{nodeIdx, 2, 0},
			move{nodeIdx, 3, 0},
			move{nodeIdx, 4, 0},
			move{nodeIdx, 5, 0},
			move{nodeIdx, 5, 1},
			move{nodeIdx, 5, 2},
			move{nodeIdx, 5, 99},
		)
	}

	// Exhaustive BFS to depth 6 for 3 nodes with arbitrary stale lag.
	bfsViolations := 0
	seen := map[indState]struct{}{indInit(): {}}
	frontier := []indState{indInit()}
	for d := 0; d < 6 && len(frontier) > 0; d++ {
		var next []indState
		for _, s := range frontier {
			for _, m := range allMoves {
				t, ok := indStep(s, m.node, m.op, m.lag, false)
				if !ok {
					continue
				}
				if t.violation() != "" {
					bfsViolations++
					continue
				}
				if _, dup := seen[t]; !dup {
					seen[t] = struct{}{}
					next = append(next, t)
				}
			}
		}
		frontier = next
	}

	// 50,000 random walks of up to 80 steps.
	rng := rand.New(rand.NewPCG(42, 99))
	walkViolations := 0
	walkSteps := 0
	for w := 0; w < 50_000; w++ {
		s := indInit()
		for step := 0; step < 80; step++ {
			var enabled []indState
			for _, m := range allMoves {
				if next, ok := indStep(s, m.node, m.op, m.lag, false); ok {
					enabled = append(enabled, next)
				}
			}
			if len(enabled) == 0 {
				break
			}
			s = enabled[rng.IntN(len(enabled))]
			walkSteps++
			if s.violation() != "" {
				walkViolations++
				break
			}
		}
	}

	// Control: allow overwrite in our 3-node model and verify violations are found immediately.
	ctrlViolations := 0
	for w := 0; w < 500; w++ {
		s := indInit()
		for step := 0; step < 40; step++ {
			var enabled []indState
			for _, m := range allMoves {
				if next, ok := indStep(s, m.node, m.op, m.lag, true); ok {
					enabled = append(enabled, next)
				}
			}
			if len(enabled) == 0 {
				break
			}
			s = enabled[rng.IntN(len(enabled))]
			if s.violation() != "" {
				ctrlViolations++
				break
			}
		}
	}

	held := bfsViolations == 0 && walkViolations == 0 && ctrlViolations > 0
	report(
		"MOD-04",
		"independent 3-node / 5-epoch / multi-slot-stale-list model (BFS depth 6 + 50k walks x 80 steps)",
		"0 violations on correct protocol; mutant control catches violations",
		fmt.Sprintf("bfsStates=%d bfsViol=%d walkSteps=%d walkViol=%d ctrlViol=%d/500",
			len(seen), bfsViolations, walkSteps, walkViolations, ctrlViolations),
		held,
	)
}

func attackMOD05() {
	// MOD-05a: multi-seed invariance at depth 10 + 10,000 deeper random walks (length 100).
	seeds := []uint64{1, 7, 42, 999, 123456}
	var baseInterleavings uint64
	var baseStates int
	invarianceOK := true
	totalWalkViol := 0
	totalWalkSteps := 0
	for i, seed := range seeds {
		r := model.Explore(model.Correct, 10, seed, 2000, 100)
		if i == 0 {
			baseInterleavings, baseStates = r.Interleavings, r.States
		} else if r.Interleavings != baseInterleavings || r.States != baseStates {
			invarianceOK = false
		}
		if r.Violating != 0 || r.WalkViolating != 0 {
			totalWalkViol += int(r.Violating) + r.WalkViolating
		}
		totalWalkSteps += r.WalkSteps
	}
	report(
		"MOD-05a",
		"model.Explore across 5 seeds (depth 10) + 10,000 random walks of 100 steps",
		"identical exhaustive counts across seeds and 0 violations",
		fmt.Sprintf("interleavings=%d states=%d invarianceOK=%t walkSteps=%d violations=%d",
			baseInterleavings, baseStates, invarianceOK, totalWalkSteps, totalWalkViol),
		invarianceOK && totalWalkViol == 0,
	)

	// MOD-05b: probe 10.1's printed bound "at most 3 epochs and 5 local commits per node".
	// Have A fence at 1 and commit 4 writes (positions 2..5), then B claims and fences at 6,
	// then B tries its very first write. Because model.go compares n.Local >= MaxLocal where
	// Fence sets n.Local = at (the absolute stream position), B gets 0 writes!
	script, err := model.ParseScript("A claim, A fence, A write, A ship, A write, A ship, A write, A ship, A write, A ship, B claim, B fence, B write")
	if err != nil {
		fatal("mod05b parse", err)
	}
	lines, finalState := model.Replay(model.Correct, script)
	lastLine := lines[len(lines)-1]
	bCouldWrite := !strings.Contains(lastLine, "refused: not enabled")
	report(
		"MOD-05b",
		"probe model bound '5 local commits per node': A commits 4 writes (pos 2..5), B fences at 6 and tries its 1st write",
		"B can perform at least 1 local commit under '5 local commits per node'",
		fmt.Sprintf("B.Local=%d, lastStep=%q", finalState.Nodes[1].Local, strings.TrimSpace(lastLine)),
		bCouldWrite,
	)
}

func main() {
	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	store, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), s3Bucket, s3Prefix)
	if err != nil {
		fatal("connect seaweedfs", err)
	}
	defer func() { _, _ = store.Wipe(ctx) }()

	attackMOD01(root)
	attackMOD02(ctx, root, store)
	attackMOD03(ctx, root)
	attackMOD04()
	attackMOD05()
}
