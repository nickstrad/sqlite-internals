// Experiment 10.1 tests the single-writer ownership invariant in two parts.
//
// Part A runs the finite-state ownership model (service/model) exhaustively up
// to a bounded depth plus seeded random walks, checking that slot exclusivity
// in the store combined with the writer's epoch/append rule prevents any fenced
// epoch from getting a commit accepted. It also runs two broken mutants
// (store-accepts-every-put and resume-from-global-end) as self-tests to show
// each half of the fencing rule is necessary and that the checker catches
// violations.
//
// Part B drives two real node child processes (a and b) sharing a WAL catalog
// and a SeaweedFS S3 store through a 15-step stale-owner scenario: a stale
// listing during takeover (fault stale-list:/log/:1), an active stale owner
// fenced on ship, a killed stale owner restarting with its persisted .owner
// belief file and getting fenced on open, and the contrast between
// store-durable acknowledgements (kept) and an unshipped local-durable
// acknowledgement (lost on takeover). Every command step is cross-checked
// against the model and against the standing descriptor and memory bounds.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/model"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

var (
	failures   []string
	depth      = flag.Int("depth", 16, "exhaustive search depth of the model")
	seed       = flag.Uint64("seed", 1, "seed of the scheduler's action order and random walks")
	walks      = flag.Int("walks", 2000, "seeded random walks past the exhaustive depth")
	walkLength = flag.Int("walk-length", 60, "steps per random walk")
	mutant     = flag.String("mutant", string(model.Correct), "model under test: correct, store-accepts-every-put, resume-from-global-end")
	modelOnly  = flag.Bool("model-only", false, "run part A only (no store needed)")
	role       = flag.String("role", "", "internal: run as a service node")
	process    node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10a", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-1", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 4, "databases a node keeps open")
}

func check(ok bool, claim string) {
	status := "ok"
	if !ok {
		status = "FAILED"
		failures = append(failures, claim)
	}
	fmt.Printf("check %s: %s\n", status, claim)
}

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func partA() {
	fmt.Println("\n== A. the ownership model, every interleaving up to a bounded depth ==")
	fmt.Printf("model: %d nodes, one database, the catalog's epoch counter, a store of position slots with put-if-absent; actions per node: claim, fence, fence-with-stale-list, write, ship, crash, restart, restart-with-stale-list; at most %d epochs, and a node's local commits stop at stream position %d (the bound counts positions across all epochs and fences, not commits per node)\n", model.Nodes, model.MaxEpoch, model.MaxLocal)
	fmt.Println("invariants after every step: no two epochs accepted for overlapping positions; no epoch accepted after a newer one was; no accepted object overwritten")
	under, err := model.ParseMutant(*mutant)
	if err != nil {
		fatal("-mutant", err)
	}
	report := model.Explore(under, *depth, *seed, *walks, *walkLength)
	fmt.Printf("%s, depth %d, seed %d: %d interleavings explored, %d distinct states, %d interleavings violate an invariant\n", report.Mutant, report.Depth, *seed, report.Interleavings, report.States, report.Violating)
	fmt.Printf("seeded random walks: %d walks, %d steps, %d reached a violation\n", report.Walks, report.WalkSteps, report.WalkViolating)
	printTrace(report)
	check(report.Violating == 0 && report.WalkViolating == 0 && report.Shortest == nil, fmt.Sprintf("model %s: no interleaving up to depth %d and no random walk lets a fenced epoch get a commit accepted", report.Mutant, *depth))
	// Self-tests: disable each half of the fencing rule in turn so the
	// experiment proves the checker can fail when either guarantee is missing.
	for _, m := range []model.Mutant{model.StoreAcceptsEveryPut, model.ResumeFromGlobalEnd} {
		if m == under {
			continue
		}
		self := model.Explore(m, *depth, *seed, 0, 0)
		fmt.Printf("self-test %s: %d of %d interleavings violate an invariant\n", m, self.Violating, self.Interleavings)
		printTrace(self)
		check(self.Violating > 0 && self.Shortest != nil, fmt.Sprintf("self-test: with %s the checker finds the violation", m))
	}
}

func printTrace(report model.Report) {
	if report.Shortest == nil {
		return
	}
	fmt.Printf("  shortest violating interleaving (%d steps): %s\n", len(report.Shortest), report.Violation)
	for _, line := range report.Shortest {
		fmt.Println("    " + line)
	}
}

type step struct {
	node, command string
	model         map[string]string
}

// scenario scripts two real nodes alongside the model actions for each step:
// steps 1-7 exercise stale-list retry, active stale-owner fencing, and loss of
// an unshipped local-durable commit on db-0001; steps 8-15 exercise SIGKILL and
// restart of a stale owner holding a persisted belief file on db-0002 while
// keeping ownership of db-0001.
var scenario = []step{
	{"a", "claim db-0001", map[string]string{"db-0001": "A claim, A fence"}},
	{"a", "write db-0001 store-durable", map[string]string{"db-0001": "A write, A ship"}},
	{"a", "write db-0001 local-durable", map[string]string{"db-0001": "A write"}},
	{"b", "claim db-0001", map[string]string{"db-0001": "B claim, B fence-with-stale-list, B fence"}},
	{"a", "write db-0001 store-durable", map[string]string{"db-0001": "A write, A ship"}},
	{"a", "write db-0001 local-durable", map[string]string{"db-0001": "A write"}},
	{"b", "write db-0001 store-durable", map[string]string{"db-0001": "B write, B ship"}},
	{"b", "claim db-0002", map[string]string{"db-0002": "B claim, B fence"}},
	{"b", "write db-0002 store-durable", map[string]string{"db-0002": "B write, B ship"}},
	{"a", "claim db-0002", map[string]string{"db-0002": "A claim, A fence"}},
	{"b", "kill", map[string]string{"db-0001": "B crash", "db-0002": "B crash"}},
	{"b", "restart", map[string]string{"db-0001": "B restart", "db-0002": "B restart"}},
	{"b", "write db-0002 store-durable", map[string]string{"db-0002": "B write, B ship"}},
	{"b", "write db-0001 store-durable", map[string]string{"db-0001": "B write, B ship"}},
	{"a", "write db-0002 store-durable", map[string]string{"db-0002": "A write, A ship"}},
}

var databases = []string{"db-0001", "db-0002"}

type ack struct {
	database           string
	mode               node.AckMode
	epoch, userVersion int
	position           int64
}

func lastNumber(text string) int64 {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r < '0' || r > '9' })
	var n int64 = -1
	if len(fields) > 0 {
		fmt.Sscan(fields[len(fields)-1], &n)
	}
	return n
}

func agrees(reply string, outcomes []string) bool {
	last := outcomes[len(outcomes)-1]
	if strings.HasPrefix(reply, "refused ") || !strings.HasPrefix(last, "ok") {
		return strings.HasPrefix(reply, "refused ") && strings.HasPrefix(last, "refused")
	}
	fences := 0
	for _, o := range outcomes {
		if strings.Contains(o, "fence at") || strings.HasSuffix(o, ", retry") {
			fences++
		}
	}
	if at := strings.Index(reply, "fence at position"); at >= 0 {
		return reply[at:] == fmt.Sprintf("fence at position %d, attempts %d", lastNumber(last), fences)
	}
	if strings.Contains(reply, "stored through position") {
		return lastNumber(reply) == lastNumber(last)
	}
	return true
}

func startNode(ctx context.Context, work, name, fault string) *node.Process {
	config := process
	config.Name, config.Work, config.Fault = name, work, fault
	p, err := node.StartProcess(ctx, name, config.Args()...)
	if err != nil {
		fatal("start node "+name, err)
	}
	return p
}

func partB(ctx context.Context, store *s3store.Store) {
	fmt.Println("\n== B. the stale-owner scenario on two service processes against the S3 store ==")
	work := sqlitex.MustWorkDir("10.1")
	if _, err := store.Wipe(ctx); err != nil {
		fatal("wipe the store prefix", err)
	}
	shown := work
	if root, err := sqlitex.RepoRoot(); err == nil {
		if relative, err := filepath.Rel(root, work); err == nil {
			shown = relative
		}
	}
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, shown)
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("catalog", err)
	}
	defer cat.Close()
	for _, id := range databases {
		if err := cat.Register(ctx, id, node.PlatformSettings()); err != nil {
			fatal("register", err)
		}
	}
	fmt.Printf("store: bucket %s, prefix %s; catalog: %s registered with %s; nodes a and b share the catalog; each keeps at most %d databases open\n",
		store.Bucket, store.Prefix, strings.Join(databases, ", "), node.PlatformSettings(), process.Capacity)
	fmt.Println("node b's store wrapper hides the newest slot from its first listing (fault stale-list:/log/:1)")
	nodes := map[string]*node.Process{"a": startNode(ctx, work, "a", ""), "b": startNode(ctx, work, "b", "stale-list:/log/:1")}
	states := map[string]model.State{}
	for _, id := range databases {
		states[id] = model.Initial()
	}
	var acks []ack
	var problems, overBounds []string
	commands, agreed, sampled := 0, 0, 0
	for i, s := range scenario {
		fmt.Printf("step %d: node %s: %s\n", i+1, s.node, s.command)
		var reply node.Reply
		switch s.command {
		case "kill":
			if err := nodes[s.node].Kill(); err != nil {
				fatal("kill", err)
			}
		case "restart":
			nodes[s.node] = startNode(ctx, work, s.node, "")
		default:
			if reply, err = nodes[s.node].Send(s.command); err != nil {
				fatal("send", err)
			}
			fmt.Println("  " + reply.Line)
			fmt.Printf("INFO: %s %s\n", s.node, reply.Bounds)
			sampled++
			problems = append(problems, reply.Problems...)
			if !reply.WithinBounds() {
				overBounds = append(overBounds, reply.Bounds)
			}
		}
		for _, id := range databases {
			if _, ok := s.model[id]; !ok {
				continue
			}
			actions, err := model.ParseScript(s.model[id])
			if err != nil {
				fatal("script", err)
			}
			var outcomes, shown []string
			for _, a := range actions {
				next, outcome, enabled := model.Step(states[id], model.Correct, a)
				if !enabled {
					outcome = "refused: not enabled"
				} else {
					states[id] = next
				}
				outcomes = append(outcomes, outcome)
				shown = append(shown, a.String()+": "+outcome)
			}
			fmt.Printf("  model %s: %s\n", id, strings.Join(shown, "; "))
			if reply.Line != "" {
				commands++
				if agrees(reply.Line, outcomes) {
					agreed++
				} else {
					fmt.Println("  the model DISAGREES with this step")
				}
			}
		}
		var a ack
		if n, _ := fmt.Sscanf(reply.Line, "ok write %s %s acknowledged at epoch %d, user_version %d,", &a.database, &a.mode, &a.epoch, &a.userVersion); n == 4 {
			a.mode = node.AckMode(strings.TrimSuffix(string(a.mode), ":"))
			if a.mode == node.StoreDurable {
				a.position = lastNumber(reply.Line)
			}
			acks = append(acks, a)
		} else if strings.HasPrefix(reply.Line, "ok write ") {
			fatal("parse acknowledgement", fmt.Errorf("%q", reply.Line))
		}
	}
	check(commands == agreed, fmt.Sprintf("the real processes and the model agree at every command step (%d of %d)", agreed, commands))
	for _, name := range []string{"a", "b"} {
		reply, err := nodes[name].Close()
		if err != nil {
			fatal("close node "+name, err)
		}
		fmt.Printf("node %s: %s; %s\n", name, reply.Line, reply.Summary)
		sampled++
		problems = append(problems, reply.Problems...)
		if !reply.WithinBounds() {
			overBounds = append(overBounds, reply.Bounds)
		}
	}
	for _, problem := range problems {
		fmt.Println("problem: " + problem)
	}
	for _, line := range overBounds {
		fmt.Println("over bounds: " + line)
	}
	check(len(problems) == 0, "no node reported a problem (a failed final ship, an eviction that did not leave the shipped state at rest, an I/O error)")
	check(len(overBounds) == 0, fmt.Sprintf("after each of the %d commands the node's descriptors and memory stayed within the standing bounds", sampled))
	verify(ctx, store, cat, work, states, acks)
}

func verify(ctx context.Context, store *s3store.Store, cat *catalog.Catalog, work string, states map[string]model.State, acks []ack) {
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	fmt.Println("\nthe store after the run (read back from S3), and the model's store after the same steps:")
	for _, id := range databases {
		chain, restorable, err := fence.Audit(ctx, store, id)
		if err != nil {
			fatal("audit "+id, err)
		}
		var slots []string
		for _, slot := range chain {
			slots = append(slots, fmt.Sprintf("positions %d-%d epoch %d %s", slot.First, slot.Last, slot.Epoch, slot.Kind))
		}
		fmt.Printf("%s store: %s\n%s model: %s\n", id, strings.Join(slots, "; "), id, strings.Join(states[id].ChainLines(), "; "))
		// Require len(restorable.Damaged) == 0 in addition to RestoreAt(last):
		// a damaged segment before a newer snapshot would not affect restoring
		// the latest position, but would break point-in-time auditability.
		check(strings.Join(slots, "; ") == strings.Join(states[id].ChainLines(), "; ") && len(restorable.Damaged) == 0, id+": the store holds exactly the slots the model predicts: one lineage, epochs never decreasing, no damaged object")
		restoreAt := func(position int64) []byte {
			image, err := fence.RestoreAt(restorable, position)
			if err != nil {
				fatal(fmt.Sprintf("restore %s at %d", id, position), err)
			}
			return image
		}
		last := chain[len(chain)-1]
		image := restoreAt(last.Last)
		match := recipe.MatchHashes(hashes, recipe.HashImage(image))
		record, err := cat.Get(ctx, id)
		if err != nil {
			fatal("catalog "+id, err)
		}
		fmt.Printf("%s catalog: owner %s, epoch %d, last shipped %d, schema version %d, location %s; restore of position %d: %s\n",
			id, record.Owner, record.Epoch, record.LastShipped, record.SchemaVersion, record.Location, last.Last, match)
		check(record.Epoch == last.Epoch && record.LastShipped == last.Last && record.SchemaVersion == match.Step && match.Found() && (match.Exact || recipe.ChangeCountersAgree(image)),
			id+": the catalog names the newest epoch and the last stored position, and the restored state is a recipe state")
		status, err := connmgr.CheckAtRest(filepath.Join(work, record.Owner, id+".db"))
		if err != nil {
			fatal("at rest", err)
		}
		check(status.AtRest && status.SHA256 == walship.SHA256Hex(image), id+": the owner's closed local file is byte-identical (E1) to the state rebuilt from the store")
		for _, a := range acks {
			if a.database != id {
				continue
			}
			found := false
			for _, slot := range chain {
				for p := slot.First; p <= slot.Last && int(slot.Epoch) == a.epoch; p++ {
					if binary.BigEndian.Uint32(restoreAt(p)[60:64]) == uint32(a.userVersion) && (a.mode == node.LocalDurable || p == a.position) {
						found = true
					}
				}
			}
			fmt.Printf("%s %s acknowledgement at epoch %d of user_version %d: in the store %t\n", id, a.mode, a.epoch, a.userVersion, found)
			if a.mode == node.StoreDurable {
				check(found, fmt.Sprintf("%s: the store-durable acknowledgement of user_version %d is stored at position %d by epoch %d", id, a.userVersion, a.position, a.epoch))
			} else {
				check(!found, fmt.Sprintf("%s: the local-durable acknowledgement of user_version %d by epoch %d was never shipped, and the takeover lost it, as that mode allows", id, a.userVersion, a.epoch))
			}
		}
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	// Dispatch child-node role before printing the pin line so the child's
	// stdout begins cleanly with "ready" for node.StartProcess.
	if *role == "node" {
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	var store *s3store.Store
	if !*modelOnly {
		var err error
		if store, err = s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), process.Bucket, process.Prefix); err != nil {
			fmt.Printf("STORE UNREACHABLE: %v\n", err)
			os.Exit(1)
		}
	}
	partA()
	if !*modelOnly {
		partB(ctx, store)
	}
	if len(failures) > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
