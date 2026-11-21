// Group 3 (manager-kills-and-limits): adversarial attacks on Experiment 8.3
// (bounded-manager, rung R5): SIGKILL mid-stream + full recovery, K=1 under
// concurrency, and K above RLIMIT_NOFILE.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/procstat"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const maxRecipeStep = 4

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(2)
}

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func dbID(i int) string { return fmt.Sprintf("db-%04d", i) }

func openRecipeDB(ctx context.Context, path string) (*sqlitex.DB, error) {
	opts := recipe.Default().WithSteps(maxRecipeStep).SQLiteOptions()
	opts.Conn.CacheSize = -256
	return sqlitex.Open(ctx, path, opts)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func populateCopies(master, dir string, n int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir "+dir, err)
	}
	for i := range n {
		if err := copyFile(master, filepath.Join(dir, dbID(i)+".db")); err != nil {
			fatal("copy "+dbID(i), err)
		}
	}
}

func makePathFor(dir string, n int) func(string) (string, error) {
	return func(id string) (string, error) {
		var idx int
		if _, err := fmt.Sscanf(id, "db-%04d", &idx); err != nil || dbID(idx) != id || idx < 0 || idx >= n {
			return "", fmt.Errorf("unknown id %q", id)
		}
		return filepath.Join(dir, id+".db"), nil
	}
}

// Child role 1: runs a bounded connmgr write/read stream and prints ACK lines
// until the parent sends SIGKILL.
func childKillStream(dir string, nDBs, capK int, seed uint64) {
	ctx := context.Background()
	r := recipe.Default().WithSteps(maxRecipeStep)
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	m, err := connmgr.New(connmgr.Options{
		Capacity: capK,
		PathFor:  makePathFor(dir, nDBs),
		Open:     openRecipeDB,
	})
	if err != nil {
		fatal("connmgr.New", err)
	}
	rng := rand.New(rand.NewPCG(seed, 8301))
	steps := make([]int, nDBs)
	out := bufio.NewWriter(os.Stdout)
	for op := 1; op <= 5000; op++ {
		idx := rng.IntN(nDBs)
		if rng.IntN(100) < 40 {
			idx = rng.IntN(max(1, nDBs/10))
		}
		h, err := m.Acquire(ctx, dbID(idx))
		if err != nil {
			fatal("acquire", err)
		}
		if steps[idx] < maxRecipeStep && rng.IntN(100) < 55 {
			next := steps[idx] + 1
			if err := recipe.ApplyStep(ctx, h.DB, plan[next]); err != nil {
				fatal("apply step", err)
			}
			steps[idx] = next
			fmt.Fprintf(out, "ACK %d %d\n", idx, next)
			out.Flush()
		} else {
			var cnt int
			if err := h.DB.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&cnt); err != nil {
				fatal("read", err)
			}
			fmt.Fprintf(out, "READ %d %d\n", idx, op)
			out.Flush()
		}
		h.Release()
	}
	_ = m.Close()
}

// Child role 2: lowers RLIMIT_NOFILE and exercises connmgr with Capacity > RLIMIT_NOFILE.
func childKOverLimit(dir string, nDBs, capK int, limit uint64) {
	if err := setNoFileLimit(limit); err != nil {
		fatal("setNoFileLimit", err)
	}
	ctx := context.Background()
	m, err := connmgr.New(connmgr.Options{
		Capacity: capK,
		PathFor:  makePathFor(dir, nDBs),
		Open:     openRecipeDB,
	})
	if err != nil {
		fatal("connmgr.New", err)
	}
	var failIdx = -1
	var failErr error
	var fdsBeforeFail, fdsAfterFail int
	for i := range nDBs {
		fdsBeforeFail, _ = procstat.DescriptorCount()
		h, err := m.Acquire(ctx, dbID(i))
		if err != nil {
			fdsAfterFail, _ = procstat.DescriptorCount()
			failIdx = i
			failErr = err
			break
		}
		var n int
		if err := h.DB.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
			h.Release()
			fdsAfterFail, _ = procstat.DescriptorCount()
			failIdx = i
			failErr = err
			break
		}
		h.Release()
	}

	// Verify already-open database 0 is still servable without opening new descriptors.
	existingOK := false
	if failIdx > 0 {
		if h, err := m.Acquire(ctx, dbID(0)); err == nil {
			var n int
			if err := h.DB.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err == nil {
				existingOK = true
			}
			h.Release()
		}
	}

	// Check whether connmgr can open another new DB automatically (all open entries are idle)
	// or if it stays wedged because len(open) < Capacity.
	autoRecovered := false
	if failIdx >= 0 && failIdx+1 < nDBs {
		if h, err := m.Acquire(ctx, dbID(failIdx+1)); err == nil {
			autoRecovered = true
			h.Release()
		}
	}

	// Check whether manual Evict of an idle entry frees 3 FDs and lets a new Acquire succeed.
	manualEvictUnblocked := false
	if failIdx > 0 {
		if _, err := m.Evict(dbID(0)); err == nil {
			if h, err := m.Acquire(ctx, dbID(failIdx)); err == nil {
				manualEvictUnblocked = true
				h.Release()
			}
		}
	}

	openLen := len(m.OpenIDs())
	stats := m.Stats()
	closeErr := m.Close()
	errText := "none"
	if failErr != nil {
		errText = strings.ReplaceAll(failErr.Error(), "\n", " ")
	}
	fmt.Printf("OVERLIMIT fail_idx=%d open_len=%d fds_before=%d fds_after=%d existing_ok=%v auto_recovered=%v manual_unblocked=%v open_failures=%d close_err=%v err=%s\n",
		failIdx, openLen, fdsBeforeFail, fdsAfterFail, existingOK, autoRecovered, manualEvictUnblocked, stats.OpenFailures, closeErr, errText)
}

// Child role 3: lowers RLIMIT_NOFILE and execs Experiment 8.3 directly.
func childRunExp83UnderLimit(repoRoot, workDir string, limit uint64) {
	if err := setNoFileLimit(limit); err != nil {
		fatal("setNoFileLimit", err)
	}
	cmd := exec.Command("go", "run", "./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager",
		"-dbs", "50", "-k", "45", "-ops", "100")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+workDir)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			fatal("exec 8.3", err)
		}
	}
	fmt.Printf("EXP83_EXIT=%d\n%s", code, string(out))
}

func main() {
	if len(os.Args) == 6 && os.Args[1] == "child-kill-stream" {
		nDBs, _ := strconv.Atoi(os.Args[3])
		capK, _ := strconv.Atoi(os.Args[4])
		seed, _ := strconv.ParseUint(os.Args[5], 10, 64)
		childKillStream(os.Args[2], nDBs, capK, seed)
		return
	}
	if len(os.Args) == 6 && os.Args[1] == "child-k-over-limit" {
		nDBs, _ := strconv.Atoi(os.Args[3])
		capK, _ := strconv.Atoi(os.Args[4])
		limit, _ := strconv.ParseUint(os.Args[5], 10, 64)
		childKOverLimit(os.Args[2], nDBs, capK, limit)
		return
	}
	if len(os.Args) == 5 && os.Args[1] == "child-exp83-limit" {
		limit, _ := strconv.ParseUint(os.Args[4], 10, 64)
		childRunExp83UnderLimit(os.Args[2], os.Args[3], limit)
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch08", "manager-kills-and-limits")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	r := recipe.Default().WithSteps(maxRecipeStep)
	stepHashes, err := r.StepHashes(ctx)
	if err != nil {
		fatal("StepHashes", err)
	}
	master := filepath.Join(scratch, "master-step0.db")
	if err := r.Build(ctx, master, 0); err != nil {
		fatal("build master", err)
	}
	selfExe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}

	// MGR-01: Kill the manager mid-stream (SIGKILL) and reopen everything — 5 rounds (OUTLINE 8.3).
	{
		const nDBs = 60
		const capK = 12
		killTargets := []int{35, 70, 110, 155, 200}
		totalNotAtRestAfterKill := 0
		integrityFailures := 0
		stepViolations := 0
		notAtRestAfterRecovery := 0
		totalAckedWrites := 0

		for round, targetOps := range killTargets {
			dir := filepath.Join(scratch, fmt.Sprintf("mgr01-round-%d", round))
			populateCopies(master, dir, nDBs)

			cmd := exec.CommandContext(ctx, selfExe, "child-kill-stream", dir, strconv.Itoa(nDBs), strconv.Itoa(capK), strconv.Itoa(round+101))
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				fatal("stdout pipe", err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				fatal("start kill-stream", err)
			}

			acked := make([]int, nDBs)
			seenOps := 0
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				line := sc.Text()
				seenOps++
				if strings.HasPrefix(line, "ACK ") {
					var idx, st int
					if _, err := fmt.Sscanf(line, "ACK %d %d", &idx, &st); err == nil && idx >= 0 && idx < nDBs {
						acked[idx] = st
						totalAckedWrites++
					}
				}
				if seenOps >= targetOps {
					if err := cmd.Process.Kill(); err != nil {
						fatal("SIGKILL child", err)
					}
					break
				}
			}
			_ = cmd.Wait()

			// Inspect at-rest status immediately after SIGKILL.
			notAtRestNow := 0
			for i := range nDBs {
				st, err := connmgr.CheckAtRest(filepath.Join(dir, dbID(i)+".db"))
				if err != nil {
					fatal("check after kill", err)
				}
				if !st.AtRest {
					notAtRestNow++
				}
			}
			totalNotAtRestAfterKill += notAtRestNow

			// Reopen every DB via a fresh manager, verify integrity_check, close all.
			recMgr, err := connmgr.New(connmgr.Options{
				Capacity: capK,
				PathFor:  makePathFor(dir, nDBs),
				Open:     openRecipeDB,
			})
			if err != nil {
				fatal("recovery manager", err)
			}
			for i := range nDBs {
				h, err := recMgr.Acquire(ctx, dbID(i))
				if err != nil {
					fatal("recovery acquire "+dbID(i), err)
				}
				var ic string
				if err := h.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil || ic != "ok" {
					integrityFailures++
				}
				h.Release()
			}
			if err := recMgr.Close(); err != nil {
				fatal("recovery close", err)
			}

			// Verify every DB is now at rest and matches an exact recipe step in [acked[i], acked[i]+1].
			for i := range nDBs {
				p := filepath.Join(dir, dbID(i)+".db")
				st, err := connmgr.CheckAtRest(p)
				if err != nil || !st.AtRest {
					notAtRestAfterRecovery++
					continue
				}
				img, err := os.ReadFile(p)
				if err != nil {
					fatal("read recovered", err)
				}
				m := recipe.MatchHashes(stepHashes, recipe.HashImage(img))
				if !m.Found() || (!m.Exact && !recipe.ChangeCountersAgree(img)) || m.Step < acked[i] || m.Step > acked[i]+1 {
					stepViolations++
				}
			}
		}

		held := totalNotAtRestAfterKill > 0 && integrityFailures == 0 && stepViolations == 0 && notAtRestAfterRecovery == 0
		report("MGR-01",
			fmt.Sprintf("SIGKILL bounded manager mid-stream across %d rounds (%d DBs, K=%d) and recover every DB", len(killTargets), nDBs, capK),
			"killed manager leaves active DBs not-at-rest; full reopen recovers 100% with integrity_check=ok, step in [acked, acked+1], and all files at rest after close",
			fmt.Sprintf("rounds=%d acked_writes=%d not_at_rest_after_kill=%d integrity_failures=%d step_violations=%d not_at_rest_after_recovery=%d",
				len(killTargets), totalAckedWrites, totalNotAtRestAfterKill, integrityFailures, stepViolations, notAtRestAfterRecovery),
			held)
	}

	// MGR-02a: Experiment 8.3 end-to-end with K = 1 (OUTLINE 8.3).
	{
		expWork := filepath.Join(scratch, "exp83-k1")
		cmd := exec.CommandContext(ctx, "go", "run", "./ch08-connection-and-database-lifecycle/experiments/8.3-bounded-manager",
			"-dbs", "40", "-k", "1", "-ops", "200")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "COURSE_WORK_DIR="+expWork)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exitCode = ee.ExitCode()
			} else {
				fatal("run 8.3 -k 1", err)
			}
		}
		outStr := stdout.String()
		ok := exitCode == 0 &&
			strings.Contains(outStr, "peak open 1") &&
			strings.Contains(outStr, "peak 3 above the baseline; samples where descriptors != 3 x open connections: 0; after Close: 0") &&
			strings.Contains(outStr, "all checks passed")
		report("MGR-02a",
			"run Experiment 8.3 with -dbs 40 -k 1 -ops 200",
			"exits 0 with peak open=1, peak descriptors=3, 0 mismatches, and all checks passed",
			fmt.Sprintf("exit=%d peak_open_1=%v peak_fds_3=%v all_passed=%v",
				exitCode, strings.Contains(outStr, "peak open 1"),
				strings.Contains(outStr, "peak 3 above the baseline"),
				strings.Contains(outStr, "all checks passed")),
			ok)
	}

	// MGR-02b: Concurrent connmgr stress with K = 1 and re-entrant OnEvict hook.
	{
		const nDBs = 12
		dir := filepath.Join(scratch, "mgr02b")
		populateCopies(master, dir, nDBs)

		var hookCalls atomic.Int64
		var mgr *connmgr.Manager
		mgr, err = connmgr.New(connmgr.Options{
			Capacity: 1,
			PathFor:  makePathFor(dir, nDBs),
			Open:     openRecipeDB,
			OnEvict: func(e connmgr.Eviction) {
				// Re-entrancy check: calling Stats() and Record() from OnEvict must never deadlock.
				_ = mgr.Stats()
				_, _ = mgr.Record(e.ID)
				hookCalls.Add(1)
			},
		})
		if err != nil {
			fatal("mgr02b New", err)
		}

		plan, err := r.Plan()
		if err != nil {
			fatal("plan", err)
		}
		dbMu := make([]sync.Mutex, nDBs)
		model := make([]int, nDBs)
		var wg sync.WaitGroup
		var errCount atomic.Int64
		for g := range 8 {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(gid+1), 8302))
				for range 20 {
					idx := rng.IntN(nDBs)
					id := dbID(idx)
					dbMu[idx].Lock()
					h, err := mgr.Acquire(ctx, id)
					if err != nil {
						dbMu[idx].Unlock()
						errCount.Add(1)
						continue
					}
					if model[idx] < maxRecipeStep && rng.IntN(100) < 30 {
						next := model[idx] + 1
						if err := recipe.ApplyStep(ctx, h.DB, plan[next]); err != nil {
							errCount.Add(1)
						} else {
							model[idx] = next
						}
					} else {
						var v int
						if err := h.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != model[idx] {
							errCount.Add(1)
						}
					}
					h.Release()
					dbMu[idx].Unlock()
				}
			}(g)
		}
		wg.Wait()
		closeErr := mgr.Close()
		stats := mgr.Stats()

		wrongFiles := 0
		for i := range nDBs {
			p := filepath.Join(dir, dbID(i)+".db")
			st, err := connmgr.CheckAtRest(p)
			if err != nil || !st.AtRest {
				wrongFiles++
				continue
			}
			img, _ := os.ReadFile(p)
			m := recipe.MatchHashes(stepHashes, recipe.HashImage(img))
			if !m.Found() || (!m.Exact && !recipe.ChangeCountersAgree(img)) || m.Step != model[i] {
				wrongFiles++
			}
		}
		held := closeErr == nil && errCount.Load() == 0 && stats.MaxOpen == 1 &&
			stats.CapacityWaits > 0 && stats.EvictedNotAtRest == 0 && stats.RefusedReopens == 0 &&
			wrongFiles == 0 && hookCalls.Load() == int64(stats.Evictions)
		report("MGR-02b",
			"8 concurrent goroutines x 20 ops over 12 DBs through connmgr with Capacity=1 and re-entrant OnEvict",
			"MaxOpen=1, CapacityWaits>0, 0 errors, 0 deadlocks, all evictions at rest, all final files match recipe",
			fmt.Sprintf("max_open=%d waits=%d evictions=%d hook_calls=%d not_at_rest=%d refused=%d errs=%d wrong_files=%d close_err=%v",
				stats.MaxOpen, stats.CapacityWaits, stats.Evictions, hookCalls.Load(),
				stats.EvictedNotAtRest, stats.RefusedReopens, errCount.Load(), wrongFiles, closeErr),
			held)
	}

	// MGR-03: K above the OS descriptor limit (OUTLINE 8.3).
	{
		if !rlimitSupported {
			report("MGR-03a",
				"run connmgr with Capacity=200 under RLIMIT_NOFILE=128 over 80 WAL DBs",
				"Linux-only Setrlimit check (skipped on non-Linux)",
				"skipped on "+runtime.GOOS,
				true)
			report("MGR-03b",
				"run Experiment 8.3 with -dbs 50 -k 45 under RLIMIT_NOFILE=128",
				"Linux-only Setrlimit check (skipped on non-Linux)",
				"skipped on "+runtime.GOOS,
				true)
		} else {
			// MGR-03a: connmgr Capacity=200 under RLIMIT_NOFILE=128 over 80 WAL DBs.
			dir := filepath.Join(scratch, "mgr03a")
			populateCopies(master, dir, 80)
			cmd := exec.CommandContext(ctx, selfExe, "child-k-over-limit", dir, "80", "200", "128")
			out, err := cmd.CombinedOutput()
			if err != nil {
				fatal("child-k-over-limit", fmt.Errorf("%v: %s", err, out))
			}
			line := strings.TrimSpace(string(out))
			var failIdx, openLen, fdsBefore, fdsAfter, openFailures int
			var existingOK, autoRecovered, manualUnblocked bool
			var closeErrStr, errMsg string
			if idx := strings.Index(line, " err="); idx >= 0 {
				errMsg = line[idx+5:]
				fmt.Sscanf(line[:idx], "OVERLIMIT fail_idx=%d open_len=%d fds_before=%d fds_after=%d existing_ok=%t auto_recovered=%t manual_unblocked=%t open_failures=%d close_err=%s",
					&failIdx, &openLen, &fdsBefore, &fdsAfter, &existingOK, &autoRecovered, &manualUnblocked, &openFailures, &closeErrStr)
			}
			// Notice: if Capacity > RLIMIT_NOFILE/3, Acquire never triggers capacity eviction once EMFILE hits
			// (because len(open) < Capacity), so auto_recovered=false — every new DB fails permanently until
			// manual Evict, even though all open entries are idle!
			// Also check whether a failed sqlitex.Open leaked any descriptors (fdsAfter == fdsBefore).
			held3a := failIdx > 0 && fdsAfter == fdsBefore && existingOK && manualUnblocked && autoRecovered && closeErrStr == "<nil>"
			report("MGR-03a",
				"run connmgr with Capacity=200 under RLIMIT_NOFILE=128 over 80 WAL DBs (all open entries idle)",
				"no fd leak on EMFILE, existing open DB stays servable, and manager automatically evicts an idle entry to keep serving new DBs",
				fmt.Sprintf("fail_at_db=%d open_len=%d fds_before=%d fds_after=%d existing_ok=%v auto_recovered=%v manual_evict_unblocked=%v close_err=%s err=%q",
					failIdx, openLen, fdsBefore, fdsAfter, existingOK, autoRecovered, manualUnblocked, closeErrStr, errMsg),
				held3a)

			// MGR-03b: Experiment 8.3 preflight check under RLIMIT_NOFILE=128 (-dbs 50 -k 45).
			expWork := filepath.Join(scratch, "exp83-limit")
			cmd = exec.CommandContext(ctx, selfExe, "child-exp83-limit", repoRoot, expWork, "128")
			out, err = cmd.CombinedOutput()
			if err != nil {
				fatal("child-exp83-limit", fmt.Errorf("%v: %s", err, out))
			}
			outStr := string(out)
			var expExit int
			fmt.Sscanf(outStr, "EXP83_EXIT=%d", &expExit)
			hasPreflightErr := strings.Contains(outStr, "FATAL descriptor limit:") && strings.Contains(outStr, "soft limit")
			report("MGR-03b",
				"run Experiment 8.3 (-dbs 50 -k 45) under RLIMIT_NOFILE=128",
				"8.3 preflight detects soft limit < 3*dbs+64 and fails cleanly with FATAL descriptor limit",
				fmt.Sprintf("exit=%d preflight_detected=%v", expExit, hasPreflightErr),
				expExit != 0 && hasPreflightErr)
		}
	}
}
