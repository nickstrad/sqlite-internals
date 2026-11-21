// Command kills-and-durability attacks Chapter 10b's Experiment 10.2
// (acknowledged-writes-survive) and durability/reopen invariants against real
// SeaweedFS S3: kills during LRU eviction, kills during fence and discontinuity
// snapshots, kills during catalog updates, pwrite64 kills mid-write, and the
// ack-before-put must-be-able-to-fail control.
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
	"runtime"
	"strings"

	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/catalog"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/lab"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/node"
	"github.com/nickstrad/sqlite-internals/ch10-multi-database-platform-capstone/service/s3store"
	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/rangefs"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
	"github.com/nickstrad/sqlite-internals/internal/walship"
)

const s3Bucket = "course-ch10b"

var (
	role    = flag.String("role", "", "internal: run as a service node child process")
	procCfg node.ProcessConfig
)

func init() {
	flag.StringVar(&procCfg.Name, "name", "", "internal: node name")
	flag.StringVar(&procCfg.Work, "work", "", "internal: work directory")
	flag.StringVar(&procCfg.Fault, "fault", "", "internal: fault rule")
	flag.StringVar(&procCfg.Bucket, "bucket", s3Bucket, "S3 bucket")
	flag.StringVar(&procCfg.Prefix, "prefix", "adv-ch10b-kills", "S3 key prefix")
	flag.IntVar(&procCfg.Capacity, "capacity", 2, "node open database capacity")
	flag.BoolVar(&procCfg.AckBeforePut, "ack-before-put", false, "internal: mutant ack-before-put")
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

func scratchDir(root, name string) string {
	dir := filepath.Join(root, "_work", "adv-ch10b", "kills-and-durability", name)
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

func resetWorkAndStore(ctx context.Context, work string, store *s3store.Store, ids []string) {
	entries, err := os.ReadDir(work)
	if err != nil {
		fatal("readdir "+work, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(work, e.Name())); err != nil {
			fatal("remove "+e.Name(), err)
		}
	}
	if _, err := store.WipeInBatches(ctx); err != nil {
		fatal("wipe s3", err)
	}
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		fatal("open catalog", err)
	}
	if err := cat.RegisterAll(ctx, ids, node.PlatformSettings()); err != nil {
		cat.Close()
		fatal("register ids", err)
	}
	if err := cat.Close(); err != nil {
		fatal("close catalog", err)
	}
}

func startNode(ctx context.Context, cfg node.ProcessConfig) *node.Process {
	p, err := node.StartProcess(ctx, cfg.Name, cfg.Args()...)
	if err != nil {
		fatal("start node "+cfg.Name, err)
	}
	return p
}

func checkCatalogIntegrity(ctx context.Context, work string) error {
	cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
	if err != nil {
		return fmt.Errorf("open catalog: %w", err)
	}
	defer cat.Close()
	var integrity string
	if err := cat.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("catalog integrity_check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("catalog integrity_check=%q", integrity)
	}
	return nil
}

// recoverAndCheck restarts node "a" (deleting a/ first in StoreDurable mode),
// reopens and inspects every database, closes cleanly, and checks integrity,
// recipe-state match, user_version >= acked, S3 lineage, and catalog integrity.
func recoverAndCheck(ctx context.Context, work, prefix string, capacity int, store *s3store.Store, hashes []recipe.FileHashes, ids []string, mode node.AckMode, acked map[string]int) ([]string, map[string]int) {
	var violations []string
	recovered := map[string]int{}
	if err := checkCatalogIntegrity(ctx, work); err != nil {
		violations = append(violations, err.Error())
	}
	if mode == node.StoreDurable {
		if err := os.RemoveAll(filepath.Join(work, "a")); err != nil {
			violations = append(violations, fmt.Sprintf("wipe a: %v", err))
			return violations, recovered
		}
	}
	cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: capacity}
	p := startNode(ctx, cfg)
	remote := node.Remote{Process: func() *node.Process { return p }}
	inspected := map[string]string{}
	for _, id := range ids {
		if mode == node.StoreDurable {
			if _, err := remote.Claim(ctx, id); err != nil {
				violations = append(violations, fmt.Sprintf("%s claim failed: %v", id, err))
				continue
			}
		}
		ins, err := remote.Inspect(ctx, id)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s reopen failed: %v", id, err))
			continue
		}
		inspected[id] = ins.Integrity
	}
	if reply, err := p.Close(); err != nil {
		violations = append(violations, fmt.Sprintf("close node a: %v", err))
	} else if !strings.HasPrefix(reply.Line, "ok ") {
		violations = append(violations, fmt.Sprintf("close reply: %s", reply.Line))
	} else if len(reply.Problems) > 0 {
		violations = append(violations, fmt.Sprintf("node problems: %s", strings.Join(reply.Problems, "; ")))
	}
	for _, id := range ids {
		integ, ok := inspected[id]
		if !ok {
			continue
		}
		if integ != "ok" {
			violations = append(violations, fmt.Sprintf("%s integrity=%s", id, integ))
			continue
		}
		dbPath := filepath.Join(work, "a", id+".db")
		status, err := connmgr.CheckAtRest(dbPath)
		if err != nil || !status.AtRest {
			violations = append(violations, fmt.Sprintf("%s not at rest: %v (%s)", id, err, status))
			continue
		}
		image, err := os.ReadFile(dbPath)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s read: %v", id, err))
			continue
		}
		state, err := lab.MatchImage(hashes, image)
		if err != nil || !state.RecipeState {
			violations = append(violations, fmt.Sprintf("%s not a recipe state: %v (%s)", id, err, state.Match))
			continue
		}
		recovered[id] = state.UserVersion
		if state.UserVersion < acked[id] {
			violations = append(violations, fmt.Sprintf("%s recovered=%d < acked=%d", id, state.UserVersion, acked[id]))
		}
		if _, _, err := fence.Audit(ctx, store, id); err != nil {
			violations = append(violations, fmt.Sprintf("%s audit: %v", id, err))
		}
	}
	return violations, recovered
}

func parseAcksFromStdout(stdout string) map[string]int {
	acked := map[string]int{}
	var tracker lab.Tracker
	for _, line := range tracker.NoteOutput("a", stdout) {
		if !strings.HasPrefix(line, "ok write ") {
			continue
		}
		id, ack, err := node.ParseAck(line)
		if err == nil {
			acked[id] = max(acked[id], ack.UserVersion)
		}
	}
	return acked
}

// attackKill01 tests 10.2's must-be-able-to-fail control (-mutant ack-before-put),
// invalid mutant rejection, and the portable (non-strace) path.
func attackKill01(ctx context.Context, root string) {
	binDir := scratchDir(root, "kill01-bin")
	expWork := scratchDir(root, "kill01-work")
	binPath := filepath.Join(binDir, "exp10.2")
	prefix := "adv-ch10b-k01"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)

	build := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./ch10-multi-database-platform-capstone/experiments/10.2-acknowledged-writes-survive")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fatal("build 10.2", fmt.Errorf("%v: %s", err, out))
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
				fatal("run 10.2", err)
			}
		}
		return code, buf.String()
	}

	// 1. Mutant ack-before-put must fail with violations naming recovered step 0 < acknowledged step 1.
	codeMut, outMut := run("-mutant", "ack-before-put", "-prefix", prefix, "-syscall-kills", "2", "-random-kills", "1")
	mutDetected := codeMut != 0 &&
		strings.Contains(outMut, "check FAILED: store-durable:") &&
		strings.Contains(outMut, "recovered step 0, acknowledged step")

	// 2. Bogus mutant must exit non-zero with FATAL -mutant.
	codeBogus, outBogus := run("-mutant", "bogus", "-prefix", prefix)
	bogusRejected := codeBogus != 0 && strings.Contains(outBogus, "FATAL -mutant:")

	// 3. Portable mode must exit 0, skip strace cleanly, and still pass the self-test.
	codePort, outPort := run("-portable", "-prefix", prefix, "-random-kills", "1")
	portOK := codePort == 0 &&
		strings.Contains(outPort, "LINUX-ONLY:") &&
		strings.Contains(outPort, "check ok: self-test:") &&
		strings.Contains(outPort, "all checks passed")

	held := mutDetected && bogusRejected && portOK
	report("KILL-01",
		"run 10.2 with -mutant ack-before-put, -mutant bogus, and -portable",
		"ack-before-put exits non-zero catching lost store-durable ack; bogus exits non-zero; portable exits 0 with self-test ok",
		fmt.Sprintf("mutantExit=%d(detected=%t) bogusExit=%d(rejected=%t) portableExit=%d(ok=%t)",
			codeMut, mutDetected, codeBogus, bogusRejected, codePort, portOK),
		held)
}

// attackKill02a injects crash-before-put and crash-after-put specifically during
// LRU eviction's final ship in node.evicted() (Capacity=1, db-0002 evicts db-0001).
func attackKill02a(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := "adv-ch10b-k02a"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001", "db-0002"}

	var allViolations []string
	casesRun := 0
	for _, faultKind := range []string{"crash-before-put", "crash-after-put"} {
		work := scratchDir(root, "kill02a-"+faultKind)
		resetWorkAndStore(ctx, work, store, ids)

		// Setup: claim both databases cleanly.
		cfgClean := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
		p := startNode(ctx, cfgClean)
		for _, id := range ids {
			if reply, err := p.Send("claim " + id); err != nil || !strings.HasPrefix(reply.Line, "ok claim ") {
				fatal("setup claim "+id, fmt.Errorf("%q %v", reply.Line, err))
			}
		}
		if _, err := p.Close(); err != nil {
			fatal("setup close", err)
		}

		// Start node "a" with fault on db-0001's position 2 (shipped only when db-0002 evicts db-0001).
		cfgFault := cfgClean
		cfgFault.Fault = faultKind + ":db-0001/log/000000000002:1"
		p = startNode(ctx, cfgFault)
		reply, err := p.Send("write db-0001 local-durable")
		if err != nil || !strings.HasPrefix(reply.Line, "ok write ") {
			fatal("local-durable write before eviction", fmt.Errorf("%q %v", reply.Line, err))
		}
		// Trigger LRU eviction of db-0001 by writing db-0002; faultstore calls os.Exit(3).
		_, evictErr := p.Send("write db-0002 store-durable")
		p.Signal()
		p.Reap()
		if evictErr == nil {
			allViolations = append(allViolations, faultKind+": eviction did not crash as injected")
			continue
		}
		casesRun++

		acked := map[string]int{"db-0001": 1, "db-0002": 0}
		// Local-durable recovery must preserve db-0001 step 1 and keep catalog/lineage intact.
		v, rec := recoverAndCheck(ctx, work, prefix, 1, store, hashes, ids, node.LocalDurable, acked)
		for _, msg := range v {
			allViolations = append(allViolations, faultKind+"(local): "+msg)
		}
		if rec["db-0001"] < 1 {
			allViolations = append(allViolations, fmt.Sprintf("%s(local): db-0001=%d < 1", faultKind, rec["db-0001"]))
		}

		// For crash-after-put, slot 2 already landed in S3, so store-durable recovery
		// (deleting a/ completely) must also recover db-0001 at step 1.
		if faultKind == "crash-after-put" {
			v2, rec2 := recoverAndCheck(ctx, work, prefix, 1, store, hashes, ids, node.StoreDurable, acked)
			for _, msg := range v2 {
				allViolations = append(allViolations, faultKind+"(store): "+msg)
			}
			if rec2["db-0001"] < 1 {
				allViolations = append(allViolations, fmt.Sprintf("%s(store): db-0001=%d < 1", faultKind, rec2["db-0001"]))
			}
		}
	}

	report("KILL-02a",
		"crash-before-put and crash-after-put during LRU eviction's final ship (Capacity=1)",
		"local-durable recovery keeps db-0001 step 1 in both cases; store-durable recovery keeps step 1 after crash-after-put; catalog and S3 lineage intact",
		fmt.Sprintf("cases=%d violations=%d detail=%q", casesRun, len(allViolations), strings.Join(allViolations, "; ")),
		casesRun == 2 && len(allViolations) == 0)
}

// attackKill02b uses strace SIGKILL on every distinct fsync during LRU eviction
// (covering WAL sync, main-DB checkpoint sync during sqlite3WalClose, and catalog sync).
func attackKill02b(ctx context.Context, root string, hashes []recipe.FileHashes) {
	if !trace.Supported {
		report("KILL-02b", "strace fsync kills during LRU eviction", "skipped on non-Linux", "non-Linux", true)
		return
	}
	prefix := "adv-ch10b-k02b"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001", "db-0002"}

	setupEvictWork := func(name string) string {
		work := scratchDir(root, name)
		resetWorkAndStore(ctx, work, store, ids)
		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
		p := startNode(ctx, cfg)
		for _, id := range ids {
			if reply, err := p.Send("claim " + id); err != nil || !strings.HasPrefix(reply.Line, "ok claim ") {
				fatal("setup claim "+id, fmt.Errorf("%q %v", reply.Line, err))
			}
		}
		if _, err := p.Close(); err != nil {
			fatal("setup close", err)
		}
		return work
	}

	// Baseline trace: write db-0001 local-durable, then write db-0002 store-durable (evicts db-0001).
	baseWork := setupEvictWork("kill02b-base")
	baseCfg := node.ProcessConfig{Name: "a", Work: baseWork, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
	cmd, err := trace.SelfCommand(baseCfg.Args()...)
	if err != nil {
		fatal("self command", err)
	}
	script := "write db-0001 local-durable\nwrite db-0002 store-durable\n"
	baseRes, err := trace.Run(ctx, trace.Options{Command: cmd, Stdin: strings.NewReader(script), Syscalls: []string{"fsync", "fdatasync"}})
	if err != nil {
		fatal("strace baseline kill02b", err)
	}

	// Collect all fsync indices and pick up to 6 covering eviction of db-0001 (after db-0001's commit)
	// including db-0001.db, db-0001.db-wal, and catalog.db-wal.
	var fsyncFiles []string
	for _, call := range baseRes.Log.Calls {
		if call.Name == "fsync" || call.Name == "fdatasync" {
			fsyncFiles = append(fsyncFiles, filepath.Base(call.Path))
		}
	}
	// First write (local-durable on db-0001) does ~2-3 fsyncs; eviction + db-0002 write does the rest.
	// Target every fsync from index 3 onward (capped at 8 evenly spaced if more).
	var killPoints []int
	startIdx := 3
	if startIdx > len(fsyncFiles) {
		startIdx = 1
	}
	if len(fsyncFiles)-startIdx+1 <= 8 {
		for k := startIdx; k <= len(fsyncFiles); k++ {
			killPoints = append(killPoints, k)
		}
	} else {
		for i := range 8 {
			k := startIdx + i*(len(fsyncFiles)-startIdx)/7
			killPoints = append(killPoints, k)
		}
	}

	var violations []string
	killedCount := 0
	var targets []string
	for _, when := range killPoints {
		work := setupEvictWork(fmt.Sprintf("kill02b-k%d", when))
		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
		c, err := trace.SelfCommand(cfg.Args()...)
		if err != nil {
			fatal("self command", err)
		}
		res, err := trace.Run(ctx, trace.Options{
			Command:  c,
			Stdin:    strings.NewReader(script),
			Syscalls: []string{"fsync", "fdatasync"},
			KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: when},
		})
		if err != nil {
			fatal("strace kill02b", err)
		}
		if res.Killed == "SIGKILL" {
			killedCount++
		}
		targets = append(targets, fmt.Sprintf("%d:%s", when, fsyncFiles[when-1]))
		acked := parseAcksFromStdout(res.Stdout)
		v, _ := recoverAndCheck(ctx, work, prefix, 1, store, hashes, ids, node.LocalDurable, acked)
		for _, msg := range v {
			violations = append(violations, fmt.Sprintf("when=%d(%s): %s", when, fsyncFiles[when-1], msg))
		}
	}

	held := killedCount == len(killPoints) && len(killPoints) > 0 && len(violations) == 0
	report("KILL-02b",
		fmt.Sprintf("strace SIGKILL at %d fsync points across LRU eviction (%s)", len(killPoints), strings.Join(targets, ",")),
		"every kill leaves both DBs reopenable (integrity ok, recipe state, >= acked) and catalog intact",
		fmt.Sprintf("totalFsyncs=%d killed=%d/%d violations=%d detail=%q", len(fsyncFiles), killedCount, len(killPoints), len(violations), strings.Join(violations, "; ")),
		held)
}

// attackKill03a kills a node before and after PutIfAbsent during Claim's fence
// snapshot (both initial claim at slot 1 and recovery/takeover claim at slot 3).
func attackKill03a(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := "adv-ch10b-k03a"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001"}

	var violations []string
	casesRun := 0

	// Part 1: crash-before-put and crash-after-put on initial Claim (slot 1).
	for _, faultKind := range []string{"crash-before-put", "crash-after-put"} {
		work := scratchDir(root, "kill03a-init-"+faultKind)
		resetWorkAndStore(ctx, work, store, ids)
		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2, Fault: faultKind + ":log/000000000001:1"}
		p := startNode(ctx, cfg)
		_, err := p.Send("claim db-0001")
		p.Signal()
		p.Reap()
		if err == nil {
			violations = append(violations, "init-"+faultKind+": claim did not crash")
			continue
		}
		casesRun++
		v, _ := recoverAndCheck(ctx, work, prefix, 2, store, hashes, ids, node.StoreDurable, map[string]int{"db-0001": 0})
		for _, msg := range v {
			violations = append(violations, "init-"+faultKind+": "+msg)
		}
	}

	// Part 2: crash-before-put and crash-after-put on recovery/takeover Claim (slot 3, after step 1).
	for _, faultKind := range []string{"crash-before-put", "crash-after-put"} {
		work := scratchDir(root, "kill03a-takeover-"+faultKind)
		resetWorkAndStore(ctx, work, store, ids)
		cfgA := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
		pa := startNode(ctx, cfgA)
		if _, err := pa.Send("claim db-0001"); err != nil {
			fatal("claim a", err)
		}
		if reply, err := pa.Send("write db-0001 store-durable"); err != nil || !strings.HasPrefix(reply.Line, "ok write ") {
			fatal("write a", fmt.Errorf("%q %v", reply.Line, err))
		}
		if _, err := pa.Close(); err != nil {
			fatal("close a", err)
		}
		// Wipe node a's disk and kill a during recovery Claim's fence snapshot at slot 3.
		if err := os.RemoveAll(filepath.Join(work, "a")); err != nil {
			fatal("wipe a", err)
		}
		cfgFault := cfgA
		cfgFault.Fault = faultKind + ":log/000000000003:1"
		pa = startNode(ctx, cfgFault)
		_, err := pa.Send("claim db-0001")
		pa.Signal()
		pa.Reap()
		if err == nil {
			violations = append(violations, "takeover-"+faultKind+": claim did not crash")
			continue
		}
		casesRun++
		v, rec := recoverAndCheck(ctx, work, prefix, 2, store, hashes, ids, node.StoreDurable, map[string]int{"db-0001": 1})
		for _, msg := range v {
			violations = append(violations, "takeover-"+faultKind+": "+msg)
		}
		if rec["db-0001"] != 1 {
			violations = append(violations, fmt.Sprintf("takeover-%s: recovered=%d want 1", faultKind, rec["db-0001"]))
		}
	}

	report("KILL-03a",
		"crash-before-put and crash-after-put during Claim fence snapshot (initial slot 1 and recovery slot 3)",
		"fresh Claim recovers cleanly after every snapshot crash, keeping step 1 and valid S3 lineage",
		fmt.Sprintf("cases=%d violations=%d detail=%q", casesRun, len(violations), strings.Join(violations, "; ")),
		casesRun == 4 && len(violations) == 0)
}

// attackKill03b kills a node before and after PutIfAbsent during a discontinuity
// snapshot emitted by openDatabase() when recovering an unshipped post-crash WAL.
func attackKill03b(ctx context.Context, root string, hashes []recipe.FileHashes) {
	prefix := "adv-ch10b-k03b"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001"}

	var violations []string
	casesRun := 0
	for _, faultKind := range []string{"crash-before-put", "crash-after-put"} {
		work := scratchDir(root, "kill03b-"+faultKind)
		resetWorkAndStore(ctx, work, store, ids)

		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
		p := startNode(ctx, cfg)
		if _, err := p.Send("claim db-0001"); err != nil {
			fatal("claim", err)
		}
		if reply, err := p.Send("write db-0001 local-durable"); err != nil || !strings.HasPrefix(reply.Line, "ok write ") {
			fatal("local-durable write", fmt.Errorf("%q %v", reply.Line, err))
		}
		// SIGKILL without closing so step 1 stays unshipped in -wal.
		p.Signal()
		p.Reap()

		// Restart with fault on slot 2: openDatabase() ships a discontinuity snapshot at slot 2!
		cfgFault := cfg
		cfgFault.Fault = faultKind + ":log/000000000002:1"
		p = startNode(ctx, cfgFault)
		_, err := p.Send("check db-0001")
		p.Signal()
		p.Reap()
		if err == nil {
			violations = append(violations, faultKind+": discontinuity snapshot did not crash")
			continue
		}
		casesRun++

		// Recover on local files; verify step 1 intact and slot 2 is a valid snapshot.
		v, rec := recoverAndCheck(ctx, work, prefix, 2, store, hashes, ids, node.LocalDurable, map[string]int{"db-0001": 1})
		for _, msg := range v {
			violations = append(violations, faultKind+"(local): "+msg)
		}
		if rec["db-0001"] != 1 {
			violations = append(violations, fmt.Sprintf("%s(local): recovered=%d want 1", faultKind, rec["db-0001"]))
		}
		chain, _, err := fence.Audit(ctx, store, "db-0001")
		if err != nil {
			violations = append(violations, faultKind+"(audit): "+err.Error())
		} else if len(chain) < 2 || chain[1].Kind != fence.KindSnapshot {
			violations = append(violations, fmt.Sprintf("%s: expected discontinuity snapshot at slot 2, got %+v", faultKind, chain))
		}

		if faultKind == "crash-after-put" {
			v2, rec2 := recoverAndCheck(ctx, work, prefix, 2, store, hashes, ids, node.StoreDurable, map[string]int{"db-0001": 1})
			for _, msg := range v2 {
				violations = append(violations, faultKind+"(store): "+msg)
			}
			if rec2["db-0001"] != 1 {
				violations = append(violations, fmt.Sprintf("%s(store): recovered=%d want 1", faultKind, rec2["db-0001"]))
			}
		}
	}

	report("KILL-03b",
		"crash-before-put and crash-after-put during openDatabase discontinuity snapshot (slot 2)",
		"restart recovers step 1 cleanly, emits valid snapshot at slot 2 without corruption, and store-durable recovery keeps step 1 after crash-after-put",
		fmt.Sprintf("cases=%d violations=%d detail=%q", casesRun, len(violations), strings.Join(violations, "; ")),
		casesRun == 2 && len(violations) == 0)
}

// attackKill03c uses strace SIGKILL at fsync points inside Claim (snapshot creation
// and restore-on-claim) to cover kills during Claim that 10.2 left out.
func attackKill03c(ctx context.Context, root string, hashes []recipe.FileHashes) {
	if !trace.Supported {
		report("KILL-03c", "strace fsync kills inside Claim", "skipped on non-Linux", "non-Linux", true)
		return
	}
	prefix := "adv-ch10b-k03c"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001"}

	setupClaim := func(name string) string {
		work := scratchDir(root, name)
		resetWorkAndStore(ctx, work, store, ids)
		return work
	}

	baseWork := setupClaim("kill03c-base")
	baseCfg := node.ProcessConfig{Name: "a", Work: baseWork, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
	cmd, err := trace.SelfCommand(baseCfg.Args()...)
	if err != nil {
		fatal("self command", err)
	}
	script := "claim db-0001\n"
	baseRes, err := trace.Run(ctx, trace.Options{Command: cmd, Stdin: strings.NewReader(script), Syscalls: []string{"fsync", "fdatasync"}})
	if err != nil {
		fatal("strace baseline kill03c", err)
	}
	var fsyncFiles []string
	for _, call := range baseRes.Log.Calls {
		if call.Name == "fsync" || call.Name == "fdatasync" {
			fsyncFiles = append(fsyncFiles, filepath.Base(call.Path))
		}
	}
	killPoints := []int{1}
	if len(fsyncFiles) >= 3 {
		killPoints = append(killPoints, len(fsyncFiles)/2, len(fsyncFiles))
	} else if len(fsyncFiles) == 2 {
		killPoints = append(killPoints, 2)
	}

	var violations []string
	killedCount := 0
	for _, when := range killPoints {
		work := setupClaim(fmt.Sprintf("kill03c-k%d", when))
		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
		c, err := trace.SelfCommand(cfg.Args()...)
		if err != nil {
			fatal("self command", err)
		}
		res, err := trace.Run(ctx, trace.Options{
			Command:  c,
			Stdin:    strings.NewReader(script),
			Syscalls: []string{"fsync", "fdatasync"},
			KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: when},
		})
		if err != nil {
			fatal("strace kill03c", err)
		}
		if res.Killed == "SIGKILL" {
			killedCount++
		}
		// Recover via StoreDurable (re-claim after killed claim) and verify catalog + DB + S3 lineage.
		v, _ := recoverAndCheck(ctx, work, prefix, 2, store, hashes, ids, node.StoreDurable, map[string]int{"db-0001": 0})
		for _, msg := range v {
			violations = append(violations, fmt.Sprintf("when=%d(%s): %s", when, fsyncFiles[when-1], msg))
		}
	}

	held := killedCount == len(killPoints) && len(killPoints) > 0 && len(violations) == 0
	report("KILL-03c",
		fmt.Sprintf("strace SIGKILL at %d fsync points inside Claim (of %d total fsyncs)", len(killPoints), len(fsyncFiles)),
		"catalog stays integrity ok and subsequent Claim recovers db-0001 cleanly with valid S3 lineage",
		fmt.Sprintf("killed=%d/%d violations=%d detail=%q", killedCount, len(killPoints), len(violations), strings.Join(violations, "; ")),
		held)
}

// attackKill04 specifically targets every catalog.db-wal / catalog.db fsync
// during store-durable writes and verifies both catalog integrity and database
// recovery in both local-durable and store-durable modes, plus subsequent catalog sync.
func attackKill04(ctx context.Context, root string, hashes []recipe.FileHashes) {
	if !trace.Supported {
		report("KILL-04", "strace SIGKILL on every catalog.db fsync", "skipped on non-Linux", "non-Linux", true)
		return
	}
	prefix := "adv-ch10b-k04"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001", "db-0002"}

	setupWork := func(name string) string {
		work := scratchDir(root, name)
		resetWorkAndStore(ctx, work, store, ids)
		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
		p := startNode(ctx, cfg)
		for _, id := range ids {
			if reply, err := p.Send("claim " + id); err != nil || !strings.HasPrefix(reply.Line, "ok claim ") {
				fatal("setup claim "+id, fmt.Errorf("%q %v", reply.Line, err))
			}
		}
		if _, err := p.Close(); err != nil {
			fatal("setup close", err)
		}
		return work
	}

	baseWork := setupWork("kill04-base")
	baseCfg := node.ProcessConfig{Name: "a", Work: baseWork, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
	cmd, err := trace.SelfCommand(baseCfg.Args()...)
	if err != nil {
		fatal("self command", err)
	}
	script := "write db-0001 store-durable\nwrite db-0002 store-durable\nwrite db-0001 store-durable\n"
	baseRes, err := trace.Run(ctx, trace.Options{Command: cmd, Stdin: strings.NewReader(script), Syscalls: []string{"fsync", "fdatasync"}})
	if err != nil {
		fatal("strace baseline kill04", err)
	}

	var catalogFsyncs []int
	fsyncIdx := 0
	for _, call := range baseRes.Log.Calls {
		if call.Name == "fsync" || call.Name == "fdatasync" {
			fsyncIdx++
			base := filepath.Base(call.Path)
			if strings.HasPrefix(base, "catalog.db") {
				catalogFsyncs = append(catalogFsyncs, fsyncIdx)
			}
		}
	}

	var violations []string
	killedCount := 0
	for _, when := range catalogFsyncs {
		for _, recMode := range []node.AckMode{node.LocalDurable, node.StoreDurable} {
			work := setupWork(fmt.Sprintf("kill04-k%d-%s", when, recMode))
			cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 2}
			c, err := trace.SelfCommand(cfg.Args()...)
			if err != nil {
				fatal("self command", err)
			}
			res, err := trace.Run(ctx, trace.Options{
				Command:  c,
				Stdin:    strings.NewReader(script),
				Syscalls: []string{"fsync", "fdatasync"},
				KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: when},
			})
			if err != nil {
				fatal("strace kill04", err)
			}
			if res.Killed == "SIGKILL" {
				killedCount++
			}
			acked := parseAcksFromStdout(res.Stdout)
			v, _ := recoverAndCheck(ctx, work, prefix, 2, store, hashes, ids, recMode, acked)
			for _, msg := range v {
				violations = append(violations, fmt.Sprintf("when=%d(%s): %s", when, recMode, msg))
			}

			// Verify a follow-up store-durable write succeeds and brings catalog.last_shipped == S3 end.
			p := startNode(ctx, cfg)
			reply, err := p.Send("write db-0001 store-durable")
			if err != nil || !strings.HasPrefix(reply.Line, "ok write ") {
				violations = append(violations, fmt.Sprintf("when=%d(%s) follow-up write: %q %v", when, recMode, reply.Line, err))
			}
			if _, err := p.Close(); err != nil {
				violations = append(violations, fmt.Sprintf("when=%d(%s) follow-up close: %v", when, recMode, err))
			}
			cat, err := catalog.Open(ctx, filepath.Join(work, "catalog.db"))
			if err != nil {
				violations = append(violations, fmt.Sprintf("when=%d(%s) open cat: %v", when, recMode, err))
				continue
			}
			rec, err := cat.Get(ctx, "db-0001")
			cat.Close()
			chain, _, auditErr := fence.Audit(ctx, store, "db-0001")
			if err != nil || auditErr != nil || len(chain) == 0 || rec.LastShipped != chain[len(chain)-1].Last {
				violations = append(violations, fmt.Sprintf("when=%d(%s) catalog drift: lastShipped=%d s3End=%d err=%v/%v",
					when, recMode, rec.LastShipped, chain[len(chain)-1].Last, err, auditErr))
			}
		}
	}

	wantKills := len(catalogFsyncs) * 2
	held := len(catalogFsyncs) > 0 && killedCount == wantKills && len(violations) == 0
	report("KILL-04",
		fmt.Sprintf("strace SIGKILL on every catalog.db* fsync (%d points x 2 recovery modes)", len(catalogFsyncs)),
		"catalog stays integrity ok, both local and store recovery preserve all acked writes, and next write resyncs catalog.last_shipped",
		fmt.Sprintf("catalogFsyncs=%v killed=%d/%d violations=%d detail=%q", catalogFsyncs, killedCount, wantKills, len(violations), strings.Join(violations, "; ")),
		held)
}

// attackKill05 goes beyond 10.2's fsync-only sweep by injecting strace SIGKILL
// at pwrite64 calls (mid-WAL-frame and mid-catalog write, before fsync).
func attackKill05(ctx context.Context, root string, hashes []recipe.FileHashes) {
	if !trace.Supported {
		report("KILL-05", "strace SIGKILL at pwrite64 calls", "skipped on non-Linux", "non-Linux", true)
		return
	}
	prefix := "adv-ch10b-k05"
	store := connectStore(ctx, prefix)
	defer store.WipeInBatches(ctx)
	ids := []string{"db-0001", "db-0002"}

	setupWork := func(name string) string {
		work := scratchDir(root, name)
		resetWorkAndStore(ctx, work, store, ids)
		cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
		p := startNode(ctx, cfg)
		for _, id := range ids {
			if reply, err := p.Send("claim " + id); err != nil || !strings.HasPrefix(reply.Line, "ok claim ") {
				fatal("setup claim "+id, fmt.Errorf("%q %v", reply.Line, err))
			}
		}
		if _, err := p.Close(); err != nil {
			fatal("setup close", err)
		}
		return work
	}

	var violations []string
	totalKills, killedCount := 0, 0
	const pointsPerMode = 4
	for _, mode := range []node.AckMode{node.LocalDurable, node.StoreDurable} {
		script := fmt.Sprintf("write db-0001 %s\nwrite db-0002 %s\nwrite db-0001 %s\nwrite db-0002 %s\n", mode, mode, mode, mode)
		baseWork := setupWork("kill05-base-" + string(mode))
		baseCfg := node.ProcessConfig{Name: "a", Work: baseWork, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
		cmd, err := trace.SelfCommand(baseCfg.Args()...)
		if err != nil {
			fatal("self command", err)
		}
		baseRes, err := trace.Run(ctx, trace.Options{Command: cmd, Stdin: strings.NewReader(script), Syscalls: []string{"pwrite64"}})
		if err != nil {
			fatal("strace pwrite64 baseline", err)
		}
		pwriteCount := 0
		for _, call := range baseRes.Log.Calls {
			if call.Name == "pwrite64" {
				pwriteCount++
			}
		}
		if pwriteCount == 0 {
			violations = append(violations, fmt.Sprintf("%s: saw 0 pwrite64 calls", mode))
			continue
		}
		for i := range pointsPerMode {
			when := 1 + i*(pwriteCount-1)/(pointsPerMode-1)
			totalKills++
			work := setupWork(fmt.Sprintf("kill05-%s-%d", mode, when))
			cfg := node.ProcessConfig{Name: "a", Work: work, Bucket: s3Bucket, Prefix: prefix, Capacity: 1}
			c, err := trace.SelfCommand(cfg.Args()...)
			if err != nil {
				fatal("self command", err)
			}
			res, err := trace.Run(ctx, trace.Options{
				Command:  c,
				Stdin:    strings.NewReader(script),
				Syscalls: []string{"pwrite64"},
				KillAt:   &trace.KillAt{Syscalls: []string{"pwrite64"}, When: when},
			})
			if err != nil {
				fatal("strace pwrite64 kill", err)
			}
			if res.Killed == "SIGKILL" {
				killedCount++
			}
			acked := parseAcksFromStdout(res.Stdout)
			v, _ := recoverAndCheck(ctx, work, prefix, 1, store, hashes, ids, mode, acked)
			for _, msg := range v {
				violations = append(violations, fmt.Sprintf("%s pwrite64#%d: %s", mode, when, msg))
			}
		}
	}

	// Touch walship import check (used indirectly through fence/lab) - ensure clean compile.
	_ = walship.ErrNotFound
	held := totalKills == pointsPerMode*2 && killedCount == totalKills && len(violations) == 0
	report("KILL-05",
		fmt.Sprintf("strace SIGKILL at %d pwrite64 points across local-durable and store-durable (Capacity=1)", totalKills),
		"walIndexRecover discards incomplete WAL frames; every DB reopens with integrity ok, recipe state, >= acked, and catalog intact",
		fmt.Sprintf("killed=%d/%d violations=%d detail=%q", killedCount, totalKills, len(violations), strings.Join(violations, "; ")),
		held)
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *role == "node" {
		runtime.LockOSThread()
		if err := node.RunProcess(ctx, procCfg); err != nil {
			fatal("node "+procCfg.Name, err)
		}
		return
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	hashes, err := recipe.Default().StepHashes(ctx)
	if err != nil {
		fatal("recipe hashes", err)
	}

	attackKill01(ctx, root)
	attackKill02a(ctx, root, hashes)
	attackKill02b(ctx, root, hashes)
	attackKill03a(ctx, root, hashes)
	attackKill03b(ctx, root, hashes)
	attackKill03c(ctx, root, hashes)
	attackKill04(ctx, root, hashes)
	attackKill05(ctx, root, hashes)
}
