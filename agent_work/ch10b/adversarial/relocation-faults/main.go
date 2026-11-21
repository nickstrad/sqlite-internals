// Command relocation-faults attacks Chapter 10b's Experiment 10.3
// (interrupted-relocation) and relocation invariants (service/relocate,
// service/node/relocate.go, service/catalog/relocation.go) against real
// SeaweedFS S3: skip-fence must-fail, interrupted uploads (partial-put and
// fail-after-put + stale-list), relocating with an open long reader, rollback
// while a dead destination stays down, uncooperative unsealed source,
// concurrent takeover (superseded), and catalog-sync after Activate recovery.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/faultstore"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/relocate"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/walfleet/internal/connmgr"
	"github.com/nickstrad/walfleet/internal/rangefs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walship"
)

const (
	s3Bucket = "course-ch10b"
	dbID     = "db-0001"
)

var (
	role    = flag.String("role", "", "internal: run as a service node child process")
	procCfg node.ProcessConfig
)

func init() {
	flag.StringVar(&procCfg.Name, "name", "", "internal: node name")
	flag.StringVar(&procCfg.Work, "work", "", "internal: work directory")
	flag.StringVar(&procCfg.Fault, "fault", "", "internal: fault rule")
	flag.StringVar(&procCfg.Bucket, "bucket", s3Bucket, "S3 bucket")
	flag.StringVar(&procCfg.Prefix, "prefix", "adv-ch10b-reloc", "S3 key prefix")
	flag.IntVar(&procCfg.Capacity, "capacity", 2, "node open database capacity")
	flag.BoolVar(&procCfg.AckBeforePut, "ack-before-put", false, "internal: unused")
}

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

func runPrefix(tag string) string {
	return fmt.Sprintf("adv-ch10b-%s-%d", tag, os.Getpid())
}

func scratchDir(root, name string) string {
	dir := filepath.Join(root, "_work", "adv-ch10b", fmt.Sprintf("relocation-faults-%d", os.Getpid()), name)
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean scratch "+name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir scratch "+name, err)
	}
	return dir
}

func connectStore(ctx context.Context, prefix string) *s3store.Store {
	s, err := s3store.Connect(ctx, rangefs.S3ConfigFromEnv(), s3Bucket, prefix)
	if err != nil {
		fatal("connect s3 "+prefix, err)
	}
	return s
}

func nextStep(ctx context.Context, db *sqlitex.DB) error {
	_, err := recipe.Default().ApplyNext(ctx, db)
	return err
}

func newNode(ctx context.Context, work, name string, cat *catalog.Catalog, store walship.Store) *node.Node {
	workload := recipe.Default()
	n, err := node.Start(node.Config{
		Name:     name,
		Dir:      filepath.Join(work, name),
		Catalog:  cat,
		Store:    store,
		Capacity: 2,
		File:     workload.SQLiteOptions().File,
		Create:   func(ctx context.Context, path string) error { return workload.Build(ctx, path, 0) },
	})
	if err != nil {
		fatal("start in-process node "+name, err)
	}
	return n
}

func setupInProc(ctx context.Context, root, name, prefix string, storeA, storeB walship.Store) (string, *s3store.Store, *catalog.Catalog, *node.Node, *node.Node) {
	work := scratchDir(root, name)
	base := connectStore(ctx, prefix)
	if _, err := base.WipeInBatches(ctx); err != nil {
		fatal("wipe s3 "+prefix, err)
	}
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open catalog", err)
	}
	if err := cat.Register(ctx, dbID, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}
	if storeA == nil {
		storeA = base
	}
	if storeB == nil {
		storeB = base
	}
	a := newNode(ctx, work, "a", cat, storeA)
	b := newNode(ctx, work, "b", cat, storeB)
	return work, base, cat, a, b
}

// prepareDB has node a claim db-0001 (slot 1), write steps 1 and 2 store-durable
// (slots 2 and 3), and write step 3 local-durable (unshipped in -wal).
func prepareDB(ctx context.Context, a *node.Node) {
	if _, err := a.Claim(ctx, dbID); err != nil {
		fatal("claim", err)
	}
	for _, mode := range []node.AckMode{node.StoreDurable, node.StoreDurable, node.LocalDurable} {
		if _, err := a.Write(ctx, dbID, mode, nextStep); err != nil {
			fatal("prepare write "+string(mode), err)
		}
	}
}

func writableCount(nodes ...*node.Node) int {
	n := 0
	for _, nd := range nodes {
		if strings.HasSuffix(nd.Status(dbID), "writable") {
			n++
		}
	}
	return n
}

// attackRel01 runs 10.3's must-be-able-to-fail control (-mutant skip-fence -case 1,
// -mutant bogus) and an in-process SkipFence proof that two active writers both get
// StoreDurable writes accepted and break S3 lineage.
func attackRel01(ctx context.Context, root string) {
	binDir := scratchDir(root, "rel01-bin")
	expWork := scratchDir(root, "rel01-work")
	binPath := filepath.Join(binDir, "exp10.3")
	prefix := runPrefix("r01")
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)

	build := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./ch10-multi-database-platform-capstone/experiments/10.3-interrupted-relocation")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fatal("build 10.3", fmt.Errorf("%v: %s", err, out))
	}

	run := func(args ...string) (int, string) {
		cmd := exec.CommandContext(ctx, binPath, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+expWork)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				fatal("run 10.3", err)
			}
		}
		return code, buf.String()
	}

	codeMut, outMut := run("-mutant", "skip-fence", "-case", "1", "-prefix", prefix)
	mutDetected := codeMut != 0 &&
		strings.Contains(outMut, "a owner at epoch 1, writable; b owner at epoch 2, writable") &&
		strings.Contains(outMut, "the store does not hold one lineage") &&
		strings.Contains(outMut, "check FAILED: case 1:")

	codeBogus, outBogus := run("-mutant", "bogus", "-prefix", prefix)
	bogusRejected := codeBogus != 0 && strings.Contains(outBogus, "FATAL -mutant:")

	// In-process SkipFence: confirm both a and b are writable and accept conflicting writes,
	// breaking S3 lineage.
	_, base, cat, a, b := setupInProc(ctx, root, "rel01-inproc", runPrefix("r01-ip"), nil, nil)
	defer base.WipeInBatches(ctx)
	defer cat.Close()
	defer a.Close()
	defer b.Close()
	prepareDB(ctx, a)

	coord := &relocate.Coordinator{
		Catalog:   cat,
		Store:     base,
		Node:      func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
		SkipFence: true,
	}
	if _, err := coord.Run(ctx, dbID, "a", "b"); err != nil {
		fatal("skip-fence run", err)
	}
	bothWritable := writableCount(a, b) == 2
	// b writes StoreDurable at slot 4 (epoch 2 segment without a fence snapshot -> breaks lineage).
	_, errB := b.Write(ctx, dbID, node.StoreDurable, nextStep)
	// a still believes it is writable and accepts a LocalDurable write on its own copy.
	_, errA := a.Write(ctx, dbID, node.LocalDurable, nextStep)
	_, _, auditErr := fence.Audit(ctx, base, dbID)
	inProcDetected := bothWritable && errB == nil && errA == nil && errors.Is(auditErr, fence.ErrBadChain)

	held := mutDetected && bogusRejected && inProcDetected
	report("REL-01",
		"10.3 -mutant skip-fence -case 1, -mutant bogus, and in-process SkipFence dual-writer proof",
		"skip-fence exposes 2 writable nodes and broken lineage (exit 1); bogus exits 1; in-process audit returns ErrBadChain",
		fmt.Sprintf("mutantExit=%d(detected=%t) bogusExit=%d(rejected=%t) inProcDualWriters=%t(auditErr=%v)",
			codeMut, mutDetected, codeBogus, bogusRejected, bothWritable && errB == nil && errA == nil, auditErr),
		held)
}

// attackRel02a injects partial-put interrupted uploads into SeaweedFS S3 during
// step 3 (final segment, slot 4) and step 6 (fence snapshot, slot 5), confirming
// Resume and Rollback never split-brain and fence.Inspect never lists the partial
// put as a restore point.
func attackRel02a(ctx context.Context, root string) {
	type tc struct {
		name      string
		faultNode string
		match     string
		wantStep  relocate.Step
		wantGood  int64
	}
	cases := []tc{
		{name: "final-segment", faultNode: "a", match: "log/000000000004", wantStep: relocate.ShipFinal, wantGood: 3},
		{name: "fence-snapshot", faultNode: "b", match: "log/000000000005", wantStep: relocate.Activate, wantGood: 4},
	}

	var problems []string
	for _, c := range cases {
		prefix := runPrefix("r02a-" + c.name)
		base := connectStore(ctx, prefix)
		defer base.WipeInBatches(ctx)
		if _, err := base.WipeInBatches(ctx); err != nil {
			fatal("wipe "+prefix, err)
		}
		work := scratchDir(root, "rel02a-"+c.name)
		cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
		if err != nil {
			fatal("open cat", err)
		}
		if err := cat.Register(ctx, dbID, node.PlatformSettings()); err != nil {
			fatal("register", err)
		}
		var storeA, storeB walship.Store = base, base
		rule := faultstore.Rule{Fault: faultstore.PartialPut, Match: c.match, Nth: 1}
		if c.faultNode == "a" {
			storeA = faultstore.New(base, rule)
		} else {
			storeB = faultstore.New(base, rule)
		}
		a := newNode(ctx, work, "a", cat, storeA)
		b := newNode(ctx, work, "b", cat, storeB)
		prepareDB(ctx, a)

		coord := &relocate.Coordinator{
			Catalog: cat,
			Store:   base,
			Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
		}
		_, runErr := coord.Run(ctx, dbID, "a", "b")
		var stepErr *relocate.StepError
		if !errors.As(runErr, &stepErr) || stepErr.Step != c.wantStep {
			problems = append(problems, fmt.Sprintf("%s: Run err=%v want step %s", c.name, runErr, c.wantStep))
		}
		if w := writableCount(a, b); w > 1 {
			problems = append(problems, fmt.Sprintf("%s: writable=%d after partial-put", c.name, w))
		}
		// Fresh coordinator with clean store: both Resume and Rollback must refuse with ErrDamaged.
		freshCoord := &relocate.Coordinator{
			Catalog: cat,
			Store:   base,
			Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
		}
		if _, resErr := freshCoord.Resume(ctx, dbID); !errors.Is(resErr, fence.ErrDamaged) {
			problems = append(problems, fmt.Sprintf("%s: Resume err=%v want ErrDamaged", c.name, resErr))
		}
		if _, rbErr := freshCoord.Rollback(ctx, dbID); !errors.Is(rbErr, fence.ErrDamaged) {
			problems = append(problems, fmt.Sprintf("%s: Rollback err=%v want ErrDamaged", c.name, rbErr))
		}
		// fence.Inspect must NOT list the partial put as a restore point and must name LastGood == wantGood.
		ins, insErr := fence.Inspect(ctx, base, dbID, nil)
		if insErr != nil || ins.Problem == "" || ins.LastGood != c.wantGood || int64(ins.RestorePoints) != c.wantGood {
			problems = append(problems, fmt.Sprintf("%s: Inspect=%+v err=%v want LastGood=%d", c.name, ins, insErr, c.wantGood))
		}
		if w := writableCount(a, b); w > 1 {
			problems = append(problems, fmt.Sprintf("%s: writable=%d after recovery attempts", c.name, w))
		}
		a.Close()
		b.Close()
		cat.Close()
	}

	report("REL-02a",
		"inject partial-put interrupted upload on final segment (slot 4) and on fence snapshot (slot 5) during relocation",
		"Run fails at injected step; Resume/Rollback refuse with ErrDamaged (<=1 writable); fence.Inspect never lists partial slot and names LastGood=3/4",
		fmt.Sprintf("cases=2 problems=%d detail=%q", len(problems), strings.Join(problems, "; ")),
		len(problems) == 0)
}

// attackRel02b leaves a complete fence upload in S3 whose reply failed (fail-after-put
// on slot 5), then runs Rollback through a stale-list coordinator store that hides slot 5.
func attackRel02b(ctx context.Context, root string) {
	prefix := runPrefix("r02b")
	base := connectStore(ctx, prefix)
	defer base.WipeInBatches(ctx)
	if _, err := base.WipeInBatches(ctx); err != nil {
		fatal("wipe "+prefix, err)
	}
	work := scratchDir(root, "rel02b")
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open cat", err)
	}
	defer cat.Close()
	if err := cat.Register(ctx, dbID, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}
	storeB := faultstore.New(base, faultstore.Rule{Fault: faultstore.FailAfterPut, Match: "log/000000000005", Nth: 1})
	a := newNode(ctx, work, "a", cat, base)
	b := newNode(ctx, work, "b", cat, storeB)
	defer a.Close()
	defer b.Close()
	prepareDB(ctx, a)

	coord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	_, runErr := coord.Run(ctx, dbID, "a", "b")
	var stepErr *relocate.StepError
	if !errors.As(runErr, &stepErr) || stepErr.Step != relocate.Activate {
		fatal("expected Activate fail-after-put", runErr)
	}

	// Now Rollback runs with a 1-shot stale-list hiding slot 5 (epoch 2 fence) from running().
	staleStore := faultstore.New(base, faultstore.Rule{Fault: faultstore.StaleList, Match: "log/", Nth: 1})
	staleCoord := &relocate.Coordinator{
		Catalog: cat,
		Store:   staleStore,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	_, rbErr := staleCoord.Rollback(ctx, dbID)
	// a.Drain must see slot 5 (epoch 2) in S3 and fail with fence.ErrFenced!
	fencedOnDrain := errors.Is(rbErr, fence.ErrFenced)

	// Next, clean Resume adopts slot 5 and finishes cleanly with b as sole owner.
	cleanCoord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	res, resErr := cleanCoord.Resume(ctx, dbID)
	chain, _, auditErr := fence.Audit(ctx, base, dbID)
	w := writableCount(a, b)
	held := fencedOnDrain && resErr == nil && res.Outcome == relocate.RolledForward && auditErr == nil && w == 1 && len(chain) == 5 && chain[len(chain)-1].Epoch == 2
	report("REL-02b",
		"fail-after-put leaves fence (slot 5) in S3, then Rollback sees stale-list hiding slot 5",
		"stale Rollback is fenced at a.Drain (ErrFenced) without split brain; subsequent Resume adopts slot 5 cleanly",
		fmt.Sprintf("staleFired=%d rbErrIsFenced=%t resumeOutcome=%s resumeErr=%v auditErr=%v writable=%d",
			len(staleStore.Fired()), fencedOnDrain, res.Outcome, resErr, auditErr, w),
		held)
}

// attackRel03 tests relocating a database while an external SQLite long reader
// holds an open read transaction (blocking WAL backfill and keeping -wal/-shm open),
// then closing the long reader and resuming the relocation.
func attackRel03(ctx context.Context, root string) {
	prefix := runPrefix("r03")
	work, base, cat, a, b := setupInProc(ctx, root, "rel03", prefix, nil, nil)
	defer base.WipeInBatches(ctx)
	defer cat.Close()
	defer a.Close()
	defer b.Close()

	// a claims db-0001 and commits steps 1 and 2 StoreDurable.
	if _, err := a.Claim(ctx, dbID); err != nil {
		fatal("claim", err)
	}
	for range 2 {
		if _, err := a.Write(ctx, dbID, node.StoreDurable, nextStep); err != nil {
			fatal("write step 1-2", err)
		}
	}

	// Open an independent long reader directly on a's local DB file and pin a
	// deferred read-only transaction (not BEGIN IMMEDIATE, which would take the
	// write lock instead of a WAL read mark).
	opts := recipe.Default().SQLiteOptions()
	opts.Conn = node.PlatformSettings()
	opts.SingleConnection = true
	opts.ImmediateTx = false
	readerDB, err := sqlitex.Open(ctx, filepath.Join(work, "a", dbID+".db"), opts)
	if err != nil {
		fatal("open long reader", err)
	}
	tx, err := readerDB.BeginTx(ctx, nil)
	if err != nil {
		readerDB.Close()
		fatal("begin long reader tx", err)
	}
	var countAtStep2 int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&countAtStep2); err != nil {
		tx.Rollback()
		readerDB.Close()
		fatal("long reader select", err)
	}

	// Commit step 3 LocalDurable on a while the long reader holds its WAL read mark at step 2.
	if _, err := a.Write(ctx, dbID, node.LocalDurable, nextStep); err != nil {
		tx.Rollback()
		readerDB.Close()
		fatal("write step 3 under long reader", err)
	}

	coord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	_, runErr := coord.Run(ctx, dbID, "a", "b")
	var stepErr *relocate.StepError
	drainBlocked := errors.As(runErr, &stepErr) && stepErr.Step == relocate.ShipFinal
	statusAWhileOpen, statusBWhileOpen := a.Status(dbID), b.Status(dbID)
	writableWhileOpen := writableCount(a, b)
	// Also test Rollback while the long reader is STILL open: must refuse cleanly without split brain.
	_, rbWhileOpenErr := coord.Rollback(ctx, dbID)
	writableAfterRbAttempt := writableCount(a, b)

	heldA := drainBlocked && writableWhileOpen == 0 && rbWhileOpenErr != nil && writableAfterRbAttempt == 0 &&
		strings.Contains(statusAWhileOpen, "sealed") && statusBWhileOpen == "not owner"
	report("REL-03a",
		fmt.Sprintf("relocate db-0001 with open long reader pinning WAL read mark (rows=%d) and step 3 committed", countAtStep2),
		"Drain refuses while reader keeps -wal/-shm open (step 3); a stays sealed, b not owner, <=1 writable; Rollback while open also refuses cleanly",
		fmt.Sprintf("runErr=%v statusA=%q statusB=%q writable=%d rbWhileOpenErr=%v",
			runErr, statusAWhileOpen, statusBWhileOpen, max(writableWhileOpen, writableAfterRbAttempt), rbWhileOpenErr),
		heldA)

	// Now release the long reader so sqlite3WalClose checkpoints and removes -wal/-shm, then Resume!
	if err := tx.Rollback(); err != nil {
		fatal("rollback long reader tx", err)
	}
	if err := readerDB.Close(); err != nil {
		fatal("close long reader", err)
	}

	// Fresh coordinator resumes from catalog row.
	freshCoord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	res, resErr := freshCoord.Resume(ctx, dbID)
	var bVersion int
	var bReadErr error
	if resErr == nil {
		bReadErr = b.Read(ctx, dbID, func(ctx context.Context, db *sqlitex.DB) error {
			return db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&bVersion)
		})
	}
	chain, restorable, auditErr := fence.Audit(ctx, base, dbID)
	var lastSHA string
	if auditErr == nil && len(chain) > 0 {
		if img, err := fence.RestoreAt(restorable, chain[len(chain)-1].Last); err == nil {
			lastSHA = walship.SHA256Hex(img)
		}
	}
	// Close b to verify at-rest file matches store's last state.
	closeBErr := b.Close()
	atRestB, atRestErr := connmgr.CheckAtRest(filepath.Join(work, "b", dbID+".db"))
	heldB := resErr == nil && res.Outcome == relocate.RolledForward && bReadErr == nil && bVersion == 3 &&
		auditErr == nil && closeBErr == nil && atRestErr == nil && atRestB.AtRest && atRestB.SHA256 == lastSHA &&
		a.Status(dbID) == "not owner"
	report("REL-03b",
		"close the long reader and Resume the previously blocked relocation",
		"Resume rolls forward cleanly, retires a, activates b at epoch 2 with user_version=3 byte-identical to store",
		fmt.Sprintf("resumeOutcome=%s resumeErr=%v bVersion=%d auditErr=%v atRestMatch=%t statusA=%q",
			res.Outcome, resErr, bVersion, auditErr, atRestErr == nil && atRestB.SHA256 == lastSHA, a.Status(dbID)),
		heldB)
}

// attackRel04 tests rolling back an interrupted relocation when the destination
// node b is DOWN (not restarted before Rollback) — the canonical reason an
// operator rolls back a migration. Uses real node child processes.
func attackRel04(ctx context.Context, root string) {
	prefix := runPrefix("r04")
	work := scratchDir(root, "rel04")
	base := connectStore(ctx, prefix)
	defer base.WipeInBatches(ctx)
	if _, err := base.WipeInBatches(ctx); err != nil {
		fatal("wipe "+prefix, err)
	}
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open cat", err)
	}
	defer cat.Close()
	if err := cat.Register(ctx, dbID, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}

	procs := map[string]*node.Process{}
	startProc := func(name string) {
		cfg := node.ProcessConfig{Name: name, Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
		p, err := node.StartProcess(ctx, name, cfg.Args()...)
		if err != nil {
			fatal("start proc "+name, err)
		}
		procs[name] = p
	}
	remote := func(name string) node.Remote {
		return node.Remote{Process: func() *node.Process { return procs[name] }}
	}
	defer func() {
		for _, p := range procs {
			if p != nil {
				p.Signal()
				p.Reap()
			}
		}
	}()

	startProc("a")
	startProc("b")
	if _, err := remote("a").Claim(ctx, dbID); err != nil {
		fatal("claim a", err)
	}
	if _, err := procs["a"].Send("write " + dbID + " store-durable"); err != nil {
		fatal("write a", err)
	}
	if _, err := procs["a"].Send("write " + dbID + " local-durable"); err != nil {
		fatal("write a local", err)
	}

	// Run relocation a -> b, stopping after step 4 (Rebuild: b has .staged, no fence in S3).
	coord := &relocate.Coordinator{
		Catalog:    cat,
		Store:      base,
		Node:       func(name string) relocate.Participant { return remote(name) },
		CrashAfter: relocate.Rebuild,
	}
	if _, err := coord.Run(ctx, dbID, "a", "b"); !errors.Is(err, relocate.ErrCrashed) {
		fatal("run until Rebuild", err)
	}

	// Kill destination b and leave it DOWN (the real reason to roll back!).
	procs["b"].Signal()
	procs["b"].Reap()
	procs["b"] = nil

	// Call Rollback while b is down!
	rbCoord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return remote(name) },
	}
	rbRes, rbErr := rbCoord.Rollback(ctx, dbID)
	relRow, _ := cat.GetRelocation(ctx, dbID)
	catRec, _ := cat.Get(ctx, dbID)
	// Try starting a replacement relocation a -> c to see if the database is stuck.
	_, newRelErr := cat.BeginRelocation(ctx, dbID, "a", "c")

	// Also see what happens if b comes back and operator retries Rollback.
	startProc("b")
	retryRes, retryErr := rbCoord.Rollback(ctx, dbID)
	_, stagedStatErr := os.Stat(filepath.Join(work, "b", dbID+".staged"))
	stagedStillPresent := stagedStatErr == nil

	held := rbErr == nil && rbRes.Outcome == relocate.RolledBack &&
		relRow.State == catalog.RelocationRolledBack &&
		catRec.Owner == "a" && newRelErr == nil
	report("REL-04",
		"kill destination b after step 4 (Rebuild) and call Rollback while b remains down",
		"Rollback succeeds (a reclaims cleanly, relocations.state=rolled-back, and a->c relocation can start)",
		fmt.Sprintf("rbErr=%v catOwner=%s/e%d relState=%s beginNextErr=%v retryRollback=(%s,%v) bStagedOrphaned=%t",
			rbErr, catRec.Owner, catRec.Epoch, relRow.State, newRelErr, retryRes.Outcome, retryErr, stagedStillPresent),
		held)
}

// attackRel05a removes a's .sealed file after b activates (simulating an
// uncooperative source ignoring the courtesy seal) and proves S3 fencing stops a.
func attackRel05a(ctx context.Context, root string) {
	prefix := runPrefix("r05a")
	work, base, cat, a, b := setupInProc(ctx, root, "rel05a", prefix, nil, nil)
	defer base.WipeInBatches(ctx)
	defer cat.Close()
	defer a.Close()
	defer b.Close()
	prepareDB(ctx, a)

	coord := &relocate.Coordinator{
		Catalog:    cat,
		Store:      base,
		Node:       func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
		CrashAfter: relocate.Activate,
	}
	if _, err := coord.Run(ctx, dbID, "a", "b"); !errors.Is(err, relocate.ErrCrashed) {
		fatal("run to Activate", err)
	}
	// Delete a's .sealed file to simulate an uncooperative source that ignored the seal.
	if err := os.Remove(filepath.Join(work, "a", dbID+".sealed")); err != nil {
		fatal("remove sealed", err)
	}
	// a now thinks it is writable at epoch 1, while b's epoch 2 fence is in S3!
	_, errA := a.Write(ctx, dbID, node.StoreDurable, nextStep)
	aFenced := errors.Is(errA, fence.ErrFenced)
	statusA := a.Status(dbID)
	ackB, errB := b.Write(ctx, dbID, node.StoreDurable, nextStep)
	chain, _, auditErr := fence.Audit(ctx, base, dbID)

	held := aFenced && statusA == "fenced" && errB == nil && ackB.Epoch == 2 && auditErr == nil && len(chain) > 0 && chain[len(chain)-1].Epoch == 2
	report("REL-05a",
		"delete a's .sealed file after b activates (uncooperative source) and attempt StoreDurable write on a",
		"S3 fencing rejects a with ErrFenced, a becomes fenced, b writes cleanly at epoch 2, lineage unbroken",
		fmt.Sprintf("errA=%v(isFenced=%t) statusA=%q errB=%v bEpoch=%d auditErr=%v",
			errA, aFenced, statusA, errB, ackB.Epoch, auditErr),
		held)
}

// attackRel05b triggers a concurrent Claim takeover by a third node c during an
// interrupted relocation a -> b (both before and after Activate) and checks that
// Resume and Rollback detect Superseded and fence both a and b.
func attackRel05b(ctx context.Context, root string) {
	var problems []string
	for _, crashStep := range []relocate.Step{relocate.ShipFinal, relocate.Activate} {
		prefix := runPrefix(fmt.Sprintf("r05b-s%d", int(crashStep)))
		work, base, cat, a, b := setupInProc(ctx, root, fmt.Sprintf("rel05b-s%d", int(crashStep)), prefix, nil, nil)
		defer base.WipeInBatches(ctx)
		c := newNode(ctx, work, "c", cat, base)
		prepareDB(ctx, a)

		coord := &relocate.Coordinator{
			Catalog:    cat,
			Store:      base,
			Node:       func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
			CrashAfter: crashStep,
		}
		if _, err := coord.Run(ctx, dbID, "a", "b"); !errors.Is(err, relocate.ErrCrashed) {
			problems = append(problems, fmt.Sprintf("step %d: run err=%v", crashStep, err))
		}
		// Third node c takes over with Claim (epoch 3 fence).
		claimC, err := c.Claim(ctx, dbID)
		if err != nil || claimC.Epoch != 3 {
			problems = append(problems, fmt.Sprintf("step %d: c.Claim=%+v err=%v", crashStep, claimC, err))
		}
		// Resume (on step 3) or Rollback (on step 6) must return ErrSupersededBy + Outcome Superseded.
		fresh := &relocate.Coordinator{
			Catalog: cat,
			Store:   base,
			Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
		}
		var res relocate.Result
		var recErr error
		if crashStep == relocate.ShipFinal {
			res, recErr = fresh.Resume(ctx, dbID)
		} else {
			res, recErr = fresh.Rollback(ctx, dbID)
		}
		if res.Outcome != relocate.Superseded || !errors.Is(recErr, relocate.ErrSupersededBy) {
			problems = append(problems, fmt.Sprintf("step %d: recovery=(%s,%v) want Superseded", crashStep, res.Outcome, recErr))
		}
		// Neither a nor b may get a StoreDurable write accepted into S3.
		if _, err := a.Write(ctx, dbID, node.StoreDurable, nextStep); err == nil {
			problems = append(problems, fmt.Sprintf("step %d: a wrote after c took over", crashStep))
		}
		if _, err := b.Write(ctx, dbID, node.StoreDurable, nextStep); err == nil {
			problems = append(problems, fmt.Sprintf("step %d: b wrote after c took over", crashStep))
		}
		// c continues writing cleanly at epoch 3.
		if ack, err := c.Write(ctx, dbID, node.StoreDurable, nextStep); err != nil || ack.Epoch != 3 {
			problems = append(problems, fmt.Sprintf("step %d: c write failed: %+v %v", crashStep, ack, err))
		}
		if _, _, err := fence.Audit(ctx, base, dbID); err != nil {
			problems = append(problems, fmt.Sprintf("step %d: audit err=%v", crashStep, err))
		}
		a.Close()
		b.Close()
		c.Close()
		cat.Close()
	}

	report("REL-05b",
		"third node c executes Claim takeover during interrupted relocation a->b (after step 3 and after step 6)",
		"Resume/Rollback report Superseded (ErrSupersededBy), neither a nor b writes to S3, c continues at epoch 3",
		fmt.Sprintf("cases=2 problems=%d detail=%q", len(problems), strings.Join(problems, "; ")),
		len(problems) == 0)
}

// hookStore fires onPostPut once after PutIfAbsent succeeds on a matching key.
type hookStore struct {
	walship.Store
	match     string
	once      sync.Once
	onPostPut func()
}

func (h *hookStore) PutIfAbsent(ctx context.Context, key string, data []byte) error {
	err := h.Store.PutIfAbsent(ctx, key, data)
	if err == nil && strings.Contains(key, h.match) {
		h.once.Do(h.onPostPut)
	}
	return err
}

// attackRel05c forces b.Activate's Catalog.RecordShipped to fail with SQLITE_BUSY
// AFTER b already placed the fence snapshot in S3 and wrote db-0001.owner, then
// releases the catalog lock and calls Resume to check whether the catalog is
// brought up to date (location, last_shipped, schema_version) when Resume finishes.
func attackRel05c(ctx context.Context, root string) {
	prefix := runPrefix("r05c")
	work := scratchDir(root, "rel05c")
	base := connectStore(ctx, prefix)
	defer base.WipeInBatches(ctx)
	if _, err := base.WipeInBatches(ctx); err != nil {
		fatal("wipe "+prefix, err)
	}
	catPath := filepath.Join(work, "catalog.db")
	cat, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("open cat", err)
	}
	defer cat.Close()
	if err := cat.Register(ctx, dbID, node.PlatformSettings()); err != nil {
		fatal("register", err)
	}

	// Second handle to catalog.db that acquires a BEGIN IMMEDIATE write lock right
	// after b puts fence slot 5, holding it longer than catalog's 5s busy_timeout
	// so b.Activate fails at recordShipped AFTER writeBelief and n.owned[id]=2.
	blockerCat, err := catalog.Open(ctx, catPath)
	if err != nil {
		fatal("open blocker catalog", err)
	}
	defer blockerCat.Close()
	var blockerWg sync.WaitGroup
	releaseBlocker := make(chan struct{})
	locked := make(chan struct{})

	storeB := &hookStore{
		Store: base,
		match: "log/000000000005",
		onPostPut: func() {
			blockerWg.Add(1)
			go func() {
				defer blockerWg.Done()
				tx, err := blockerCat.DB.BeginTx(ctx, nil)
				if err != nil {
					close(locked)
					return
				}
				_, _ = tx.ExecContext(ctx, "UPDATE databases SET location = location WHERE id = ?", dbID)
				close(locked)
				select {
				case <-releaseBlocker:
				case <-time.After(8 * time.Second):
				}
				_ = tx.Rollback()
			}()
			<-locked
		},
	}

	a := newNode(ctx, work, "a", cat, base)
	b := newNode(ctx, work, "b", cat, storeB)
	defer a.Close()
	defer b.Close()
	prepareDB(ctx, a)

	coord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	_, runErr := coord.Run(ctx, dbID, "a", "b")
	close(releaseBlocker)
	blockerWg.Wait()

	var stepErr *relocate.StepError
	failedAtActivate := errors.As(runErr, &stepErr) && stepErr.Step == relocate.Activate
	// Now catalog is unlocked; Resume should adopt and finish cleanly.
	freshCoord := &relocate.Coordinator{
		Catalog: cat,
		Store:   base,
		Node:    func(name string) relocate.Participant { return map[string]*node.Node{"a": a, "b": b}[name] },
	}
	res, resErr := freshCoord.Resume(ctx, dbID)
	rec, err := cat.Get(ctx, dbID)
	if err != nil {
		fatal("catalog get", err)
	}
	relRow, err := cat.GetRelocation(ctx, dbID)
	if err != nil {
		fatal("relocation get", err)
	}

	// Expected after completed relocation (state=done): catalog tracks owner=b, epoch=2,
	// location="b:db-0001.db", last_shipped=5 (fence slot), schema_version=3.
	catalogSynced := rec.Owner == "b" && rec.Epoch == 2 && rec.Location == "b:"+dbID+".db" && rec.LastShipped == 5 && rec.SchemaVersion == 3
	held := failedAtActivate && resErr == nil && res.Outcome == relocate.RolledForward && relRow.State == catalog.RelocationDone && catalogSynced
	report("REL-05c",
		"fail b.Activate at recordShipped (SQLITE_BUSY) after PutIfAbsent+writeBelief, then unlock catalog and Resume",
		"Resume completes (state=done) and repairs catalog to owner=b epoch=2 location=b:db-0001.db last_shipped=5 schema_version=3",
		fmt.Sprintf("failedAtActivate=%t resume=(%s,%v) relState=%s cat=(owner=%s epoch=%d loc=%q lastShipped=%d schemaVer=%d)",
			failedAtActivate, res.Outcome, resErr, relRow.State, rec.Owner, rec.Epoch, rec.Location, rec.LastShipped, rec.SchemaVersion),
		held)
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		if err := node.RunProcess(ctx, procCfg); err != nil {
			fatal("node "+procCfg.Name, err)
		}
		return
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	defer os.RemoveAll(filepath.Join(root, "_work", "adv-ch10b", fmt.Sprintf("relocation-faults-%d", os.Getpid())))

	attackRel01(ctx, root)
	attackRel02a(ctx, root)
	attackRel02b(ctx, root)
	attackRel03(ctx, root)
	attackRel04(ctx, root)
	attackRel05a(ctx, root)
	attackRel05b(ctx, root)
	attackRel05c(ctx, root)
}
