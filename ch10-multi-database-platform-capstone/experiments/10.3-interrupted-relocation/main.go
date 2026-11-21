package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/relocate"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const id = "db-0001"

var (
	failures []string
	mutant   = flag.String("mutant", "", "skip-fence: relocate without sealing the source and without the store fence")
	only     = flag.Int("case", 0, "run only this case number (0 runs every case)")
	role     = flag.String("role", "", "internal: run as a service node")
	process  node.ProcessConfig
)

func init() {
	flag.StringVar(&process.Name, "name", "", "internal: node name")
	flag.StringVar(&process.Work, "work", "", "internal: work directory")
	flag.StringVar(&process.Fault, "fault", "", "internal: fault rule fault:match:nth on the node's store")
	flag.StringVar(&process.Bucket, "bucket", "course-ch10b", "S3 bucket")
	flag.StringVar(&process.Prefix, "prefix", "exp-10-3", "key prefix of this experiment in the bucket")
	flag.IntVar(&process.Capacity, "capacity", 2, "databases a node keeps open")
	flag.BoolVar(&process.AckBeforePut, "ack-before-put", false, "internal: unused here")
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

type faultCase struct {
	crashAfter relocate.Step
	kill       string
	killAfter  relocate.Step
	faultNode  string
	fault      string
	rollback   bool
}

const finalSegment, fenceSlot = "log/000000000004", "log/000000000006"

func (c faultCase) interrupts() bool {
	return !(c.kill == "b" && c.killAfter == relocate.Activate)
}

func (c faultCase) String() string {
	var what string
	switch {
	case c.crashAfter != 0:
		what = fmt.Sprintf("the coordinator crashes after step %s", c.crashAfter)
	case c.kill != "":
		what = fmt.Sprintf("node %s is killed after step %s", c.kill, c.killAfter)
	case c.fault != "":
		what = fmt.Sprintf("store fault %s on node %s", c.fault, c.faultNode)
	default:
		what = "no fault"
	}
	recovery := "resume"
	if c.rollback {
		recovery = "roll back"
	}
	return what + "; recovery: " + recovery
}

func cases() []faultCase {
	var list []faultCase
	for _, step := range relocate.Steps() {
		list = append(list, faultCase{crashAfter: step})
	}
	for _, step := range relocate.Steps()[:relocate.Activate] {
		list = append(list, faultCase{crashAfter: step, rollback: true})
	}
	for _, k := range []struct {
		name  string
		after relocate.Step
	}{{"a", relocate.FenceEpoch}, {"a", relocate.SealSource}, {"a", relocate.ShipFinal}, {"b", relocate.Rebuild}, {"b", relocate.Activate}, {"a", relocate.Activate}} {
		list = append(list, faultCase{kill: k.name, killAfter: k.after})
	}
	list = append(list, faultCase{kill: "a", killAfter: relocate.SealSource, rollback: true}, faultCase{kill: "b", killAfter: relocate.Rebuild, rollback: true})
	for _, f := range []string{"fail-before-put", "fail-after-put", "crash-before-put", "crash-after-put"} {
		list = append(list, faultCase{faultNode: "a", fault: f + ":" + finalSegment + ":1"}, faultCase{faultNode: "b", fault: f + ":" + fenceSlot + ":1"})
	}
	list = append(list, faultCase{faultNode: "a", fault: "fail-before-put:" + finalSegment + ":1", rollback: true}, faultCase{faultNode: "b", fault: "crash-before-put:" + fenceSlot + ":1", rollback: true})
	return list
}

type run struct {
	env           *lab.Env
	skipFence     bool
	nodes         map[string]*node.Process
	acks          []node.Ack
	refused       int
	mostWritable  int
	boundaryLines []string
}

func (r *run) remote(name string) node.Remote {
	return node.Remote{
		Process: func() *node.Process { return r.nodes[name] },
		OnReply: func(_ string, reply node.Reply) { r.env.Tracker.Note(name, reply) },
	}
}

func (r *run) start(name, fault string) {
	p, err := r.env.Start(r.env.Config(name, fault))
	if err != nil {
		fatal("start node "+name, err)
	}
	r.nodes[name] = p
}

func (r *run) kill(name string) {
	if p := r.nodes[name]; p != nil {
		p.Signal()
		p.Reap()
	}
	r.nodes[name] = nil
}

func (r *run) alive(name string) bool {
	if r.nodes[name] == nil {
		return false
	}
	if _, err := r.remote(name).Status(id); err != nil {
		r.kill(name)
		return false
	}
	return true
}

func (r *run) write(name string, mode node.AckMode) {
	if !r.alive(name) {
		r.refused++
		return
	}
	reply, err := r.nodes[name].Send(fmt.Sprintf("write %s %s", id, mode))
	if err != nil {
		r.kill(name)
		r.refused++
		return
	}
	r.env.Tracker.Note(name, reply)
	if strings.HasPrefix(reply.Line, "refused ") {
		r.refused++
		return
	}
	_, ack, err := node.ParseAck(reply.Line)
	if err != nil {
		fatal("parse acknowledgement", err)
	}
	r.acks = append(r.acks, ack)
}

func (r *run) boundary(label string) {
	writable := 0
	var states []string
	for _, name := range []string{"a", "b"} {
		state := "down"
		if r.alive(name) {
			status, err := r.remote(name).Status(id)
			if err != nil {
				fatal("status", err)
			}
			state = status
			if strings.HasSuffix(status, "writable") {
				writable++
			}
		}
		states = append(states, name+" "+state)
	}
	r.mostWritable = max(r.mostWritable, writable)
	record, err := r.env.Catalog.Get(r.env.Ctx, id)
	if err != nil {
		fatal("catalog", err)
	}
	storeEnd := "the store is not one lineage"
	if chain, err := fence.Chain(r.env.Ctx, r.env.Store, id); err == nil && len(chain) > 0 {
		end := chain[len(chain)-1]
		storeEnd = fmt.Sprintf("store ends at position %d (epoch %d %s)", end.Last, end.Epoch, end.Kind)
	}
	r.boundaryLines = append(r.boundaryLines, fmt.Sprintf("%s: catalog owner %s epoch %d; %s; %s", label, record.Owner, record.Epoch, storeEnd, strings.Join(states, "; ")))
	other := "a"
	if record.Owner == "a" {
		other = "b"
	}
	r.write(record.Owner, node.StoreDurable)
	r.write(other, node.LocalDurable)
}

func short(digest string) string {
	if len(digest) < 12 {
		return digest
	}
	return digest[:12]
}

func (r *run) coordinator() *relocate.Coordinator {
	return &relocate.Coordinator{Catalog: r.env.Catalog, Store: r.env.Store, Node: func(name string) relocate.Participant { return r.remote(name) }, SkipFence: r.skipFence}
}

func describe(err error) string {
	var stepErr *relocate.StepError
	switch {
	case errors.Is(err, relocate.ErrCrashed):
		return "the coordinator stopped"
	case errors.As(err, &stepErr):
		var refused *node.RefusedError
		if errors.As(err, &refused) {
			return fmt.Sprintf("step %d failed: the node refused", int(stepErr.Step))
		}
		return fmt.Sprintf("step %d failed: the node did not answer", int(stepErr.Step))
	}
	return "error: " + err.Error()
}

func runCase(env *lab.Env, number int, c faultCase, skipFence bool) (bool, int) {
	fmt.Printf("\ncase %d: %s\n", number, c)
	if err := env.Reset([]string{id}); err != nil {
		fatal("reset", err)
	}
	r := &run{env: env, nodes: map[string]*node.Process{}, skipFence: skipFence}
	for _, name := range []string{"a", "b"} {
		fault := ""
		if c.faultNode == name {
			fault = c.fault
		}
		r.start(name, fault)
	}
	if _, err := r.remote("a").Claim(env.Ctx, id); err != nil {
		fatal("claim", err)
	}
	r.write("a", node.StoreDurable)
	r.write("a", node.StoreDurable)
	r.write("a", node.LocalDurable)
	coordinator := r.coordinator()
	coordinator.CrashAfter = c.crashAfter
	coordinator.AfterStep = func(step relocate.Step) {
		r.boundary(fmt.Sprintf("after step %d", int(step)))
		if c.kill != "" && step == c.killAfter {
			r.kill(c.kill)
		}
	}
	result, err := coordinator.Run(env.Ctx, id, "a", "b")
	interrupted := err != nil
	if err != nil {
		fmt.Printf("  run: %s\n", describe(err))
	} else {
		fmt.Printf("  run: finished without interruption (%s)\n", result.Outcome)
	}
	for _, name := range []string{"a", "b"} {
		if !r.alive(name) {
			fmt.Printf("  node %s is down; restarted\n", name)
			r.start(name, "")
		}
	}
	recovery := r.coordinator()
	if interrupted && c.rollback {
		result, err = recovery.Rollback(env.Ctx, id)
	} else if interrupted {
		result, err = recovery.Resume(env.Ctx, id)
	}
	if err != nil && !errors.Is(err, relocate.ErrFenceInStore) {
		fmt.Printf("  recovery: %v\n", err)
		return false, r.mostWritable
	}
	relocation, err := env.Catalog.GetRelocation(env.Ctx, id)
	if err != nil {
		fatal("relocation record", err)
	}
	fmt.Printf("  recovery: %s; relocation record: state %s, step %d; fence point: position %d sha256 %s, fence at position %d\n", result.Outcome, relocation.State, relocation.Step, result.Position, short(result.SHA256), result.Fence)
	r.boundary("after recovery")
	for _, line := range r.boundaryLines {
		fmt.Println("  " + line)
	}
	return r.verify(c, interrupted, result, relocation), r.mostWritable
}

func (r *run) verify(c faultCase, interrupted bool, result relocate.Result, relocation catalog.Relocation) bool {
	env := r.env
	for name, p := range r.nodes {
		if _, err := env.Stop(p); err != nil {
			fatal("stop node "+name, err)
		}
	}
	record, err := env.Catalog.Get(env.Ctx, id)
	if err != nil {
		fatal("catalog", err)
	}
	chain, restorable, err := fence.Audit(env.Ctx, env.Store, id)
	if err != nil {
		fmt.Printf("  the store does not hold one lineage: %v\n", err)
		return false
	}
	var slots []string
	for _, slot := range chain {
		slots = append(slots, fmt.Sprintf("%d-%d e%d %s", slot.First, slot.Last, slot.Epoch, slot.Kind))
	}
	fmt.Printf("  store: %s\n", strings.Join(slots, "; "))
	last := chain[len(chain)-1]
	restore := func(position int64) []byte {
		image, err := fence.RestoreAt(restorable, position)
		if err != nil {
			fatal(fmt.Sprintf("restore at %d", position), err)
		}
		return image
	}
	final := restore(last.Last)
	finalState, err := env.MatchImage(final)
	if err != nil {
		fatal("match", err)
	}
	owner := "b"
	wantState := catalog.RelocationDone
	if result.Outcome == relocate.RolledBack {
		owner, wantState = "a", catalog.RelocationRolledBack
	}
	fencePosition, fenceSHA := relocation.Position, relocation.SHA256
	if result.Outcome == relocate.RolledBack {
		fencePosition, fenceSHA = result.Position, result.SHA256
	}
	if fencePosition < 1 || result.Fence != fencePosition+1 {
		fmt.Printf("  the recovery names fence point %d and fence %d\n", fencePosition, result.Fence)
		return false
	}
	atFence, fenced := restore(fencePosition), restore(result.Fence)
	storeDurable := 0
	acksInLineage := true
	for _, ack := range r.acks {
		if ack.Mode != node.StoreDurable {
			continue
		}
		storeDurable++
		found := false
		for _, slot := range chain {
			if slot.Epoch == ack.Epoch && slot.First <= ack.Position && ack.Position <= slot.Last && int(restore(ack.Position)[63]) == ack.UserVersion {
				found = true
			}
		}
		acksInLineage = acksInLineage && found
	}
	status, err := env.AtRestSHA256(owner, id)
	if err != nil {
		fatal("owner file", err)
	}
	fmt.Printf("  final: catalog owner %s epoch %d, newest slot epoch %d; %d acknowledged (%d store-durable), %d refused; final user_version %d (%s); most writable nodes at any boundary %d\n",
		record.Owner, record.Epoch, last.Epoch, len(r.acks), storeDurable, r.refused, finalState.UserVersion, finalState.Match, r.mostWritable)
	parts := []struct {
		ok   bool
		what string
	}{
		{interrupted == c.interrupts(), "the fault interrupted the relocation, or (a destination killed after activation) did not"},
		{relocation.State == wantState && record.Owner == owner && record.Epoch == last.Epoch, "one owner: the catalog, the newest slot and the relocation record agree"},
		{r.mostWritable <= 1, "at most one node accepted writes at every step boundary"},
		{walship.SHA256Hex(atFence) == fenceSHA && walship.SHA256Hex(fenced) == fenceSHA, "the rebuilt copy equals the source at the fenced commit"},
		{acksInLineage && finalState.RecipeState && finalState.UserVersion == len(r.acks), "every acknowledged write is in the one lineage and no refused write is"},
		{status == walship.SHA256Hex(final), "the owner's closed file equals the store's last state"},
	}
	ok := true
	for _, part := range parts {
		if !part.ok {
			fmt.Println("  NOT: " + part.what)
			ok = false
		}
	}
	return ok
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		if err := node.RunProcess(ctx, process); err != nil {
			fatal("node "+process.Name, err)
		}
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *mutant != "" && *mutant != "skip-fence" {
		fatal("-mutant", fmt.Errorf("unknown mutant %q (want skip-fence)", *mutant))
	}
	store, err := lab.Connect(ctx, process)
	if err != nil {
		fmt.Printf("STORE UNREACHABLE: %v\n", err)
		os.Exit(1)
	}
	env, err := lab.New(ctx, "10.3", store, process)
	if err != nil {
		fatal("work directory", err)
	}
	defer env.Close()
	fmt.Printf("INFO: store %s, work directory %s\n", store.Endpoint, env.ShownWork())
	fmt.Printf("store: bucket %s, prefix %s; nodes a (source) and b (destination) share the catalog; settings %s\n", store.Bucket, store.Prefix, node.PlatformSettings())
	fmt.Println("relocation steps:")
	for _, step := range relocate.Steps() {
		fmt.Printf("  %s\n", step)
	}
	fmt.Println("each case: a claims db-0001 and writes steps 1-2 store-durable and step 3 local-durable; at every step boundary the client writes store-durable to the node the catalog names and local-durable to the other; one fault per case; then resume or roll back")
	if *mutant != "" {
		fmt.Printf("mutant: %s\n", *mutant)
	}
	passed, total := 0, 0
	for i, c := range cases() {
		if *only != 0 && *only != i+1 {
			continue
		}
		total++
		ok, _ := runCase(env, i+1, c, *mutant == "skip-fence")
		check(ok, fmt.Sprintf("case %d: one owner, at most one writable node at every boundary, the rebuilt copy equals the source at the fence, every acknowledged write kept and no refused write present", i+1))
		if ok {
			passed++
		}
	}
	fmt.Printf("\n%d of %d fault cases hold every invariant\n", passed, total)
	if *mutant == "" && *only == 0 {
		fmt.Println("\n== self-test: the same relocation without the seal and without the store fence ==")
		_, writable := runCase(env, 0, faultCase{}, true)
		check(writable >= 2, "self-test: without the fence the boundary check finds two writable nodes")
	}
	for _, problem := range env.Tracker.Problems {
		fmt.Println("problem: " + problem)
	}
	for _, line := range env.Tracker.OverBounds {
		fmt.Println("over bounds: " + line)
	}
	check(len(env.Tracker.OverBounds) == 0 && env.Tracker.Replies > 0, "after every command of every node process the descriptors and memory stayed within the standing bounds")
	fmt.Printf("INFO: %d node replies carried a bounds check\n", env.Tracker.Replies)
	if len(failures) > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
