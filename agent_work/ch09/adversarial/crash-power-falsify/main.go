package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
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
	"time"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
	"github.com/nickstrad/walfleet/internal/walfile"
)

const (
	modeRandomWriter = "child-random-writer"
	modeCkptWriter   = "child-ckpt-writer"
	modeRecover      = "child-recover"
	modePinnedNormal = "child-pinned-normal"

	baseStep        = 3
	lastStep        = 11
	checkpointAfter = 7

	// Scaled to 1,200 real SIGKILL iterations (400 per synchronous level) across 4 workers
	// to fit well within the < 10 minute total suite budget while still exercising over a thousand kills.
	randomKillsPerLevel = 400
)

func report(id, tried, expect, got string, held bool) {
	status := "BROKEN"
	if held {
		status = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, status)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

func copyFile(from, to string, limit int64) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	var src io.Reader = in
	if limit >= 0 {
		src = io.LimitReader(in, limit)
	}
	_, copyErr := io.Copy(out, src)
	return errors.Join(copyErr, out.Sync(), out.Close())
}

// runRandomWriterChild commits steps 4..11 (with a PASSIVE checkpoint after step 7)
// on one connection, writing unbuffered "READY\n" and "ack K\n" lines to stdout so
// the parent knows the exact last acknowledged commit when SIGKILL lands.
func runRandomWriterChild(args []string) error {
	fs := flag.NewFlagSet(modeRandomWriter, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	level := fs.String("sync", "FULL", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	opts := r.SQLiteOptions()
	opts.Conn.Synchronous = *level
	opts.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, *dbPath, opts)
	if err != nil {
		return err
	}
	_, _ = os.Stdout.WriteString("READY\n")
	for k := baseStep + 1; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			return err
		}
		_, _ = os.Stdout.WriteString(fmt.Sprintf("ack %d\n", k))
		if k == checkpointAfter {
			if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointPassive); err != nil {
				return err
			}
		}
	}
	// Exit without closing to mirror a process crash even if the timer missed.
	os.Exit(0)
	return nil
}

// runCkptWriterChild commits steps 4..7, then runs a checkpoint of the requested mode
// (PASSIVE or TRUNCATE), leaving the process unclosed on exit.
func runCkptWriterChild(args []string) error {
	fs := flag.NewFlagSet(modeCkptWriter, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	level := fs.String("sync", "FULL", "")
	mode := fs.String("mode", sqlitex.CheckpointPassive, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	opts := r.SQLiteOptions()
	opts.Conn.Synchronous = *level
	opts.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, *dbPath, opts)
	if err != nil {
		return err
	}
	for k := baseStep + 1; k <= checkpointAfter; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			return err
		}
	}
	if _, err := sqlitex.Checkpoint(ctx, db, "", *mode); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}

// runRecoverChild opens a crashed WAL database (no -shm), forces WAL recovery
// with a query, and closes cleanly.
func runRecoverChild(args []string) error {
	fs := flag.NewFlagSet(modeRecover, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	db, err := r.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
		_ = db.Close()
		return err
	}
	return db.Close()
}

// runPinnedNormalChild runs steps 4..7 at synchronous=NORMAL, holds a read
// transaction on a second connection at step 4 so a PASSIVE checkpoint only
// backfills partially and cannot restart the WAL, then commits steps 8..9 into
// the SAME WAL generation and exits without closing.
func runPinnedNormalChild(args []string) error {
	fs := flag.NewFlagSet(modePinnedNormal, flag.ContinueOnError)
	dbPath := fs.String("db", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runtime.LockOSThread()
	ctx := context.Background()
	r := recipe.Default()
	opts := r.SQLiteOptions()
	opts.SingleConnection = false
	opts.ImmediateTx = false
	opts.Conn.Synchronous = "NORMAL"
	opts.Conn.WALAutocheckpoint = 0
	db, err := sqlitex.Open(ctx, *dbPath, opts)
	if err != nil {
		return err
	}
	writer, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	reader, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	applyOnConn := func(k int) error {
		step, err := r.Step(k)
		if err != nil {
			return err
		}
		if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if err := step.ExecIn(ctx, writer); err != nil {
			_, _ = writer.ExecContext(ctx, "ROLLBACK")
			return err
		}
		_, err = writer.ExecContext(ctx, "COMMIT")
		return err
	}
	// Commit step 4, pin reader at step 4 so WAL cannot restart after checkpoint.
	if err := applyOnConn(4); err != nil {
		return err
	}
	if _, err := reader.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	var dummy int
	if err := reader.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&dummy); err != nil {
		return err
	}
	// Commit steps 5, 6, 7 into the same WAL generation.
	for k := 5; k <= 7; k++ {
		if err := applyOnConn(k); err != nil {
			return err
		}
	}
	// Save pre-checkpoint database file (different path so strace SyncsOf on r.db is unaffected).
	if err := copyFile(*dbPath, *dbPath+".before-ckpt", -1); err != nil {
		return err
	}
	// PASSIVE checkpoint syncs the ENTIRE WAL through step 7 (wal.c:2277) while
	// backfilling up to step 4's read mark. Because nBackfill < mxFrame, the WAL
	// does NOT restart on subsequent commits, and r.db is NOT synced (wal.c:2328-2335).
	ckpt, err := sqlitex.Checkpoint(ctx, writer, "", sqlitex.CheckpointPassive)
	if err != nil {
		return err
	}
	wal7, err := walfile.ParseFile(*dbPath + "-wal")
	if err != nil {
		return err
	}
	end7 := int64(walfile.HeaderSize + wal7.LastCommitFrame*wal7.FrameSize())
	// Commit steps 8 and 9 into the SAME WAL generation past end7.
	for k := 8; k <= 9; k++ {
		if err := applyOnConn(k); err != nil {
			return err
		}
	}
	wal9, err := walfile.ParseFile(*dbPath + "-wal")
	if err != nil {
		return err
	}
	fmt.Printf("ckpt=%s partial=%t sameGen=%t end7=%d end9=%d\n", ckpt,
		ckpt.Checkpointed > 0 && ckpt.Checkpointed < ckpt.LogFrames,
		wal7.Header.Salt1 == wal9.Header.Salt1, end7,
		int64(walfile.HeaderSize+wal9.LastCommitFrame*wal9.FrameSize()))
	os.Exit(0)
	return nil
}

// recoverAndMatch opens path (triggering SQLite recovery), runs PRAGMA integrity_check,
// closes (checkpointing to an at-rest file), and matches against precomputed step hashes.
func recoverAndMatch(ctx context.Context, r recipe.Recipe, hashes []recipe.FileHashes, path string) (integrity string, match recipe.Match, err error) {
	db, err := r.Open(ctx, path)
	if err != nil {
		return "", recipe.Match{Step: -1}, err
	}
	err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity)
	if closeErr := db.Close(); err != nil || closeErr != nil {
		return integrity, recipe.Match{Step: -1}, errors.Join(err, closeErr)
	}
	img, err := os.ReadFile(path)
	if err != nil {
		return integrity, recipe.Match{Step: -1}, err
	}
	m := recipe.MatchHashes(hashes, recipe.HashImage(img))
	if m.Found() && !m.Exact && !recipe.ChangeCountersAgree(img) {
		m = recipe.Match{Step: -1}
	}
	return integrity, m, nil
}

func maxAckFromBytes(b []byte) int {
	last := baseStep
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		var k int
		if _, err := fmt.Sscanf(line, "ack %d", &k); err == nil && k > last {
			last = k
		}
	}
	return last
}

// attackCRASH01 runs 1,200 real SIGKILL iterations (400 OFF, 400 NORMAL, 400 FULL)
// at seeded random times across 4 worker goroutines.
func attackCRASH01(ctx context.Context, self string, r recipe.Recipe, hashes []recipe.FileHashes, baseDB, work string) {
	dir := filepath.Join(work, "crash01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("crash01 mkdir", err)
	}
	defer os.RemoveAll(dir)

	baseBytes, err := os.ReadFile(baseDB)
	if err != nil {
		fatal("crash01 read base", err)
	}

	levels := []string{"OFF", "NORMAL", "FULL"}
	// Measure typical post-READY runtime per level so random kill delays land across the full run.
	durations := map[string]time.Duration{}
	for _, lv := range levels {
		calDir := filepath.Join(dir, "cal-"+lv)
		_ = os.MkdirAll(calDir, 0o755)
		dbp := filepath.Join(calDir, "r.db")
		if err := os.WriteFile(dbp, baseBytes, 0o644); err != nil {
			fatal("crash01 cal write", err)
		}
		cmd := exec.Command(self, modeRandomWriter, "-db", dbp, "-sync", lv)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fatal("crash01 cal pipe", err)
		}
		if err := cmd.Start(); err != nil {
			fatal("crash01 cal start", err)
		}
		br := bufio.NewReader(stdout)
		_, _ = br.ReadString('\n') // READY
		t0 := time.Now()
		_, _ = io.Copy(io.Discard, br)
		_ = cmd.Wait()
		d := time.Since(t0)
		if d < 5*time.Millisecond {
			d = 5 * time.Millisecond
		}
		durations[lv] = d
		_ = os.RemoveAll(calDir)
	}

	type job struct {
		level string
		idx   int
		delay time.Duration
	}
	rng := rand.New(rand.NewPCG(90201, 20261121))
	var jobs []job
	for _, lv := range levels {
		maxNS := int64(durations[lv] * 115 / 100)
		if maxNS < int64(time.Millisecond) {
			maxNS = int64(time.Millisecond)
		}
		for i := 0; i < randomKillsPerLevel; i++ {
			jobs = append(jobs, job{level: lv, idx: i, delay: time.Duration(rng.Int64N(maxNS))})
		}
	}

	var (
		totalRan     atomic.Int64
		killedMidRun atomic.Int64
		plusOneCount atomic.Int64
		failures     atomic.Int64
		mu           sync.Mutex
		firstFailure string
	)

	jobCh := make(chan job, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	workers := 4
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for j := range jobCh {
				runDir := filepath.Join(dir, fmt.Sprintf("w%d-%s-%04d", wid, j.level, j.idx))
				if err := os.MkdirAll(runDir, 0o755); err != nil {
					mu.Lock()
					if firstFailure == "" {
						firstFailure = err.Error()
					}
					mu.Unlock()
					failures.Add(1)
					continue
				}
				dbp := filepath.Join(runDir, "r.db")
				if err := os.WriteFile(dbp, baseBytes, 0o644); err != nil {
					_ = os.RemoveAll(runDir)
					failures.Add(1)
					continue
				}

				cmd := exec.Command(self, modeRandomWriter, "-db", dbp, "-sync", j.level)
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					_ = os.RemoveAll(runDir)
					failures.Add(1)
					continue
				}
				if err := cmd.Start(); err != nil {
					_ = os.RemoveAll(runDir)
					failures.Add(1)
					continue
				}

				// Read stdout concurrently while waiting for READY, then arm SIGKILL timer.
				var outBuf bytes.Buffer
				readyCh := make(chan struct{}, 1)
				readDone := make(chan struct{})
				go func() {
					defer close(readDone)
					br := bufio.NewReader(stdout)
					first := true
					for {
						line, rerr := br.ReadBytes('\n')
						if len(line) > 0 {
							outBuf.Write(line)
							if first && bytes.HasPrefix(line, []byte("READY")) {
								first = false
								readyCh <- struct{}{}
							}
						}
						if rerr != nil {
							return
						}
					}
				}()

				select {
				case <-readyCh:
					if j.delay > 0 {
						time.Sleep(j.delay)
					}
					_ = cmd.Process.Kill()
				case <-readDone:
				}
				waitErr := cmd.Wait()
				<-readDone

				totalRan.Add(1)
				if waitErr != nil {
					killedMidRun.Add(1)
				}
				lastAck := maxAckFromBytes(outBuf.Bytes())

				integ, match, recErr := recoverAndMatch(ctx, r, hashes, dbp)
				ok := recErr == nil && integ == "ok" && match.Found() && match.Step >= lastAck && match.Step <= lastAck+1
				if ok && match.Step == lastAck+1 {
					plusOneCount.Add(1)
				}
				if !ok {
					failures.Add(1)
					mu.Lock()
					if firstFailure == "" {
						firstFailure = fmt.Sprintf("%s#%d: ack=%d match=%s integ=%q err=%v", j.level, j.idx, lastAck, match, integ, recErr)
					}
					mu.Unlock()
				}
				_ = os.RemoveAll(runDir)
			}
		}(w)
	}
	wg.Wait()

	held := failures.Load() == 0 && totalRan.Load() == int64(len(jobs))
	got := fmt.Sprintf("%d/%d passed (killed_mid_run=%d, recovered_ack+1=%d, failures=%d)",
		totalRan.Load()-failures.Load(), totalRan.Load(), killedMidRun.Load(), plusOneCount.Load(), failures.Load())
	if firstFailure != "" {
		got += "; first=" + firstFailure
	}
	report("CRASH-01", fmt.Sprintf("real SIGKILL at seeded random times across %d iterations (OFF/NORMAL/FULL x %d)", len(jobs), randomKillsPerLevel),
		"every reopened DB passes integrity_check and equals a recipe step in [lastAck, lastAck+1]", got, held)
}

func writerThreadOf(res *trace.Result, syscallName string) int {
	counts := map[int]int{}
	for _, c := range res.Log.Calls {
		if c.Name == syscallName {
			counts[c.PID]++
		}
	}
	best, most := 0, 0
	for pid, n := range counts {
		if n > most {
			best, most = pid, n
		}
	}
	return best
}

// attackCRASH02 kills the process specifically on pwrite64 calls to r.db (backfill)
// and ftruncate calls during PASSIVE and TRUNCATE checkpoints (which 9.2's spread
// pwrite64 sweep never hit).
func attackCRASH02(ctx context.Context, r recipe.Recipe, hashes []recipe.FileHashes, baseDB, work string) {
	if !trace.Supported {
		report("CRASH-02", "strace SIGKILL during checkpoint r.db writes and truncates",
			"skipped on non-Linux", "LINUX-ONLY: strace unavailable on "+runtime.GOOS, true)
		return
	}
	dir := filepath.Join(work, "crash02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("crash02 mkdir", err)
	}
	defer os.RemoveAll(dir)

	type target struct {
		level string
		mode  string
	}
	targets := []target{
		{"OFF", sqlitex.CheckpointPassive},
		{"NORMAL", sqlitex.CheckpointPassive},
		{"FULL", sqlitex.CheckpointPassive},
		{"NORMAL", sqlitex.CheckpointTruncate},
		{"FULL", sqlitex.CheckpointTruncate},
	}

	tested := 0
	passed := 0
	var failDetail string

	for _, tg := range targets {
		baseDir := filepath.Join(dir, fmt.Sprintf("base-%s-%s", tg.level, tg.mode))
		dbp := filepath.Join(baseDir, "r.db")
		if err := copyFile(baseDB, dbp, -1); err != nil {
			fatal("crash02 copy", err)
		}
		cmd, err := trace.SelfCommand(modeCkptWriter, "-db", dbp, "-sync", tg.level, "-mode", tg.mode)
		if err != nil {
			fatal("crash02 cmd", err)
		}
		res, err := trace.Run(ctx, trace.Options{Command: cmd})
		if err != nil || res.ExitCode != 0 {
			fatal("crash02 baseline", fmt.Errorf("%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr))
		}
		realDB, err := filepath.EvalSymlinks(dbp)
		if err != nil {
			fatal("crash02 eval", err)
		}
		tid := writerThreadOf(res, "pwrite64")

		// Collect pwrite64 indices targeting r.db (the backfill writes!) and ftruncate indices.
		var dbWriteWhens []int
		var truncWhens []int
		pwIdx, trIdx := 0, 0
		for _, c := range res.Log.Calls {
			if c.PID != tid {
				continue
			}
			switch c.Name {
			case "pwrite64":
				pwIdx++
				if c.Path == realDB {
					dbWriteWhens = append(dbWriteWhens, pwIdx)
				}
			case "ftruncate":
				trIdx++
				if c.Path == realDB || c.Path == realDB+"-wal" {
					truncWhens = append(truncWhens, trIdx)
				}
			}
		}

		// Pick first, middle, and last r.db backfill pwrite64s, plus every ftruncate.
		var points []trace.KillAt
		if len(dbWriteWhens) > 0 {
			picks := []int{dbWriteWhens[0], dbWriteWhens[len(dbWriteWhens)/2], dbWriteWhens[len(dbWriteWhens)-1]}
			seen := map[int]bool{}
			for _, w := range picks {
				if !seen[w] {
					seen[w] = true
					points = append(points, trace.KillAt{Syscalls: []string{"pwrite64"}, When: w})
				}
			}
		}
		for _, w := range truncWhens {
			points = append(points, trace.KillAt{Syscalls: []string{"ftruncate"}, When: w})
		}

		for i, pt := range points {
			tested++
			kdir := filepath.Join(dir, fmt.Sprintf("kill-%s-%s-%d", tg.level, tg.mode, i))
			kdb := filepath.Join(kdir, "r.db")
			if err := copyFile(baseDB, kdb, -1); err != nil {
				fatal("crash02 kill copy", err)
			}
			kcmd, err := trace.SelfCommand(modeCkptWriter, "-db", kdb, "-sync", tg.level, "-mode", tg.mode)
			if err != nil {
				fatal("crash02 kill cmd", err)
			}
			kres, err := trace.Run(ctx, trace.Options{Command: kcmd, KillAt: &pt})
			if err != nil {
				fatal("crash02 kill run", err)
			}
			integ, match, recErr := recoverAndMatch(ctx, r, hashes, kdb)
			if kres.Killed == "SIGKILL" && recErr == nil && integ == "ok" && match.Found() && match.Step == checkpointAfter {
				passed++
			} else if failDetail == "" {
				failDetail = fmt.Sprintf("%s/%s %s#%d: killed=%q integ=%q match=%s err=%v", tg.level, tg.mode, pt.Syscalls[0], pt.When, kres.Killed, integ, match, recErr)
			}
		}
	}

	held := tested > 0 && passed == tested
	got := fmt.Sprintf("%d/%d mid-checkpoint kills (r.db backfill pwrite64 + ftruncate across PASSIVE/TRUNCATE) recovered step 7 with integrity=ok", passed, tested)
	if failDetail != "" {
		got += "; first failure: " + failDetail
	}
	report("CRASH-02", "SIGKILL on r.db backfill pwrite64 and ftruncate calls inside PASSIVE & TRUNCATE checkpoints",
		"every mid-checkpoint kill recovers cleanly to step 7 with integrity_check=ok", got, held)
}

// attackCRASH03 kills SQLite DURING WAL recovery (walIndexRecover) on a dirty WAL
// with no -shm, including a double-kill across two consecutive recovery attempts.
func attackCRASH03(ctx context.Context, self string, r recipe.Recipe, hashes []recipe.FileHashes, baseDB, work string) {
	if !trace.Supported {
		report("CRASH-03", "strace SIGKILL during walIndexRecover (including double-kill)",
			"skipped on non-Linux", "LINUX-ONLY: strace unavailable on "+runtime.GOOS, true)
		return
	}
	dir := filepath.Join(work, "crash03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("crash03 mkdir", err)
	}
	defer os.RemoveAll(dir)

	// Build a dirty crashed step-11 database (r.db + r.db-wal, NO -shm).
	dirtyDir := filepath.Join(dir, "dirty")
	dirtyDB := filepath.Join(dirtyDir, "r.db")
	if err := copyFile(baseDB, dirtyDB, -1); err != nil {
		fatal("crash03 copy base", err)
	}
	out, err := exec.Command(self, modeRandomWriter, "-db", dirtyDB, "-sync", "FULL").CombinedOutput()
	if err != nil {
		fatal("crash03 make dirty", fmt.Errorf("%v: %s", err, out))
	}
	_ = os.Remove(dirtyDB + "-shm")

	cloneDirty := func(dstDir string) string {
		dst := filepath.Join(dstDir, "r.db")
		if err := errors.Join(copyFile(dirtyDB, dst, -1), copyFile(dirtyDB+"-wal", dst+"-wal", -1)); err != nil {
			fatal("crash03 clone dirty", err)
		}
		return dst
	}

	// Trace one uninterrupted recovery to count pwrite64 and pread64 calls.
	baseRecDB := cloneDirty(filepath.Join(dir, "rec-base"))
	cmd, err := trace.SelfCommand(modeRecover, "-db", baseRecDB)
	if err != nil {
		fatal("crash03 rec cmd", err)
	}
	res, err := trace.Run(ctx, trace.Options{Command: cmd})
	if err != nil || res.ExitCode != 0 {
		fatal("crash03 rec base", fmt.Errorf("%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr))
	}
	tid := writerThreadOf(res, "pread64")
	preads, pwrites := 0, 0
	for _, c := range res.Log.Calls {
		if c.PID != tid {
			continue
		}
		switch c.Name {
		case "pread64":
			preads++
		case "pwrite64":
			pwrites++
		}
	}

	var points []trace.KillAt
	for j := 0; j < 6 && pwrites > 0; j++ {
		w := 1 + j*(pwrites-1)/5
		points = append(points, trace.KillAt{Syscalls: []string{"pwrite64"}, When: w})
	}
	for j := 0; j < 4 && preads > 0; j++ {
		w := 1 + j*(preads-1)/3
		points = append(points, trace.KillAt{Syscalls: []string{"pread64"}, When: w})
	}

	passed := 0
	var failDetail string
	for i, pt := range points {
		kdb := cloneDirty(filepath.Join(dir, fmt.Sprintf("rec-kill-%02d", i)))
		kcmd, err := trace.SelfCommand(modeRecover, "-db", kdb)
		if err != nil {
			fatal("crash03 kcmd", err)
		}
		kres, err := trace.Run(ctx, trace.Options{Command: kcmd, KillAt: &pt})
		if err != nil {
			fatal("crash03 krun", err)
		}
		integ, match, recErr := recoverAndMatch(ctx, r, hashes, kdb)
		if kres.Killed == "SIGKILL" && recErr == nil && integ == "ok" && match.Found() && match.Step == lastStep {
			passed++
		} else if failDetail == "" {
			failDetail = fmt.Sprintf("single %s#%d: killed=%q integ=%q match=%s err=%v", pt.Syscalls[0], pt.When, kres.Killed, integ, match, recErr)
		}
	}

	// Double-kill during recovery: kill attempt #1 on pwrite64 #1 (-shm init),
	// then kill attempt #2 on pread64 mid-WAL-scan, then let attempt #3 finish!
	doubleDB := cloneDirty(filepath.Join(dir, "rec-double"))
	pt1 := trace.KillAt{Syscalls: []string{"pwrite64"}, When: 1}
	pt2 := trace.KillAt{Syscalls: []string{"pread64"}, When: max(1, preads/2)}
	c1, _ := trace.SelfCommand(modeRecover, "-db", doubleDB)
	r1, err1 := trace.Run(ctx, trace.Options{Command: c1, KillAt: &pt1})
	c2, _ := trace.SelfCommand(modeRecover, "-db", doubleDB)
	r2, err2 := trace.Run(ctx, trace.Options{Command: c2, KillAt: &pt2})
	integD, matchD, errD := recoverAndMatch(ctx, r, hashes, doubleDB)
	doubleOK := err1 == nil && err2 == nil && r1.Killed == "SIGKILL" && r2.Killed == "SIGKILL" && errD == nil && integD == "ok" && matchD.Found() && matchD.Step == lastStep

	held := len(points) > 0 && passed == len(points) && doubleOK
	got := fmt.Sprintf("single-kill during recovery %d/%d ok (preads=%d pwrites=%d); double-kill ok=%t (step=%d, integ=%s)",
		passed, len(points), preads, pwrites, doubleOK, matchD.Step, integD)
	if failDetail != "" {
		got += "; first=" + failDetail
	}
	report("CRASH-03", "SIGKILL inside walIndexRecover (pread64/pwrite64) plus double-kill during recovery",
		"subsequent open always recovers cleanly to step 11 with integrity_check=ok", got, held)
}

// attackCRASH04 cuts the WAL file mid-header and mid-frame (both non-commit and
// commit frames) in the power-cut model and checks SQLite recovery against walfile.Replay.
func attackCRASH04(ctx context.Context, self string, r recipe.Recipe, hashes []recipe.FileHashes, baseDB, work string) {
	dir := filepath.Join(work, "crash04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("crash04 mkdir", err)
	}
	defer os.RemoveAll(dir)

	srcDir := filepath.Join(dir, "src")
	srcDB := filepath.Join(srcDir, "r.db")
	if err := copyFile(baseDB, srcDB, -1); err != nil {
		fatal("crash04 copy", err)
	}
	out, err := exec.Command(self, modeRandomWriter, "-db", srcDB, "-sync", "FULL").CombinedOutput()
	if err != nil {
		fatal("crash04 writer", fmt.Errorf("%v: %s", err, out))
	}

	wal, err := walfile.ParseFile(srcDB + "-wal")
	if err != nil {
		fatal("crash04 parse wal", err)
	}
	commits := wal.Commits()
	if len(commits) != 4 {
		fatal("crash04 commits", fmt.Errorf("expected 4 commits (steps 8..11) in restarted WAL, got %d", len(commits)))
	}
	fsz := int64(wal.FrameSize())
	c8Start := wal.Frames[commits[0].LastFrame-1].Offset
	c9End := wal.Frames[commits[1].LastFrame-1].Offset + fsz

	type cutCase struct {
		name     string
		cutBytes int64
		wantStep int
	}
	cases := []cutCase{
		{"empty-wal-0", 0, 7},
		{"mid-hdr-1", 1, 7},
		{"mid-hdr-16", 16, 7},
		{"mid-hdr-31", 31, 7},
		{"hdr-only-32", 32, 7},
		{"mid-frame1-off+1", 32 + 1, 7},
		{"mid-frame1-off+12", 32 + 12, 7},
		{"mid-frame1-off+24", 32 + 24, 7},
		{"mid-frame1-off+2048", 32 + 2048, 7},
		{"mid-frame1-off+4119", 32 + fsz - 1, 7},
		{"mid-commit8-off+1", c8Start + 1, 7},
		{"mid-commit8-off+24", c8Start + 24, 7},
		{"mid-commit8-off+2048", c8Start + 2048, 7},
		{"mid-commit8-off+4119", c8Start + fsz - 1, 7},
		{"mid-step10-after-step9", c9End + 2048, 9},
	}

	passed := 0
	var failDetail string
	for i, tc := range cases {
		cdir := filepath.Join(dir, fmt.Sprintf("cut-%02d-%s", i, tc.name))
		cdb := filepath.Join(cdir, "r.db")
		if err := errors.Join(copyFile(srcDB, cdb, -1), copyFile(srcDB+"-wal", cdb+"-wal", tc.cutBytes)); err != nil {
			fatal("crash04 cut copy", err)
		}
		// 1. Replicate via walfile.Replay before SQLite mutates cdb.
		replayOut := filepath.Join(cdir, "replayed.db")
		_, repErr := walfile.ReplayFiles(cdb, cdb+"-wal", replayOut)
		// 2. Recover via SQLite.
		integ, match, recErr := recoverAndMatch(ctx, r, hashes, cdb)
		// Compare SQLite recovered bytes with walfile.Replay bytes.
		bSQLite, _ := os.ReadFile(cdb)
		bReplay, _ := os.ReadFile(replayOut)
		e1Same := bytes.Equal(bSQLite, bReplay)

		if repErr == nil && recErr == nil && integ == "ok" && match.Found() && match.Step == tc.wantStep && e1Same {
			passed++
		} else if failDetail == "" {
			failDetail = fmt.Sprintf("%s(cut=%d): want=%d got=%s integ=%q e1Same=%t repErr=%v recErr=%v",
				tc.name, tc.cutBytes, tc.wantStep, match, integ, e1Same, repErr, recErr)
		}
	}

	held := passed == len(cases)
	got := fmt.Sprintf("%d/%d mid-header and mid-frame cuts recovered exact prior committed step, integrity=ok, and matched walfile.Replay byte-for-byte", passed, len(cases))
	if failDetail != "" {
		got += "; first=" + failDetail
	}
	report("CRASH-04", "power-cut model truncating -wal mid-header and mid-frame (non-commit and commit frames)",
		"every partial header/frame is discarded cleanly, recovering the exact prior commit and matching walfile.Replay", got, held)
}

// attackCRASH05 verifies the build-notes claim (untested in 9.2) that leaving
// old-generation WAL frames after byte 32 behind a synced restarted WAL header
// (instead of truncating to 32 bytes) still recovers cleanly to step 7 via the salt check.
func attackCRASH05(ctx context.Context, self string, r recipe.Recipe, hashes []recipe.FileHashes, baseDB, work string) {
	dir := filepath.Join(work, "crash05")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("crash05 mkdir", err)
	}
	defer os.RemoveAll(dir)

	// Run steps 4..7 + PASSIVE checkpoint to capture generation #1's WAL frames.
	gen1DB := filepath.Join(dir, "gen1", "r.db")
	if err := copyFile(baseDB, gen1DB, -1); err != nil {
		fatal("crash05 gen1 copy", err)
	}
	if out, err := exec.Command(self, modeCkptWriter, "-db", gen1DB, "-sync", "NORMAL", "-mode", sqlitex.CheckpointPassive).CombinedOutput(); err != nil {
		fatal("crash05 gen1 run", fmt.Errorf("%v: %s", err, out))
	}
	gen1WAL, err := os.ReadFile(gen1DB + "-wal")
	if err != nil {
		fatal("crash05 read gen1 wal", err)
	}

	// Run steps 4..11 to capture generation #2's restarted 32-byte header (and step-7 r.db).
	gen2DB := filepath.Join(dir, "gen2", "r.db")
	if err := copyFile(baseDB, gen2DB, -1); err != nil {
		fatal("crash05 gen2 copy", err)
	}
	if out, err := exec.Command(self, modeRandomWriter, "-db", gen2DB, "-sync", "NORMAL").CombinedOutput(); err != nil {
		fatal("crash05 gen2 run", fmt.Errorf("%v: %s", err, out))
	}
	gen2WAL, err := os.ReadFile(gen2DB + "-wal")
	if err != nil {
		fatal("crash05 read gen2 wal", err)
	}

	// Stitch [gen2 header 0..32] + [gen1 frames 32..end] — modeling a disk where the
	// synced restarted WAL header survived power loss while gen2's frame overwrites did not.
	stitched := make([]byte, len(gen1WAL))
	copy(stitched[:walfile.HeaderSize], gen2WAL[:walfile.HeaderSize])
	copy(stitched[walfile.HeaderSize:], gen1WAL[walfile.HeaderSize:])

	testDB := filepath.Join(dir, "stitched", "r.db")
	if err := copyFile(gen2DB, testDB, -1); err != nil {
		fatal("crash05 copy stitched db", err)
	}
	if err := os.WriteFile(testDB+"-wal", stitched, 0o644); err != nil {
		fatal("crash05 write stitched wal", err)
	}

	replayOut := filepath.Join(dir, "stitched", "replayed.db")
	rep, repErr := walfile.ReplayFiles(testDB, testDB+"-wal", replayOut)
	integ, match, recErr := recoverAndMatch(ctx, r, hashes, testDB)
	bSQL, _ := os.ReadFile(testDB)
	bRep, _ := os.ReadFile(replayOut)

	held := repErr == nil && recErr == nil && integ == "ok" && match.Found() && match.Step == checkpointAfter && bytes.Equal(bSQL, bRep)
	got := fmt.Sprintf("walfile validFrames=%d; SQLite recovered=%s integrity=%s; bytes==walfile.Replay:%t",
		rep.ValidFrames, match, integ, bytes.Equal(bSQL, bRep))
	report("CRASH-05", "synchronous=NORMAL power cut keeping synced gen#2 header (0..32) over stale gen#1 frames (32..end)",
		"salt mismatch stops recovery at frame 1, keeping step 7 with integrity=ok", got, held)
}

// attackCRASH06 tests 9.2's corrected portable ruleCut("NORMAL") when a
// concurrent reader prevents WAL restart after a partial PASSIVE checkpoint:
// cut is end7 (wal.c:2277), r.db has unsynced partial-backfill writes (wal.c:2328-2335),
// and both post- and pre-checkpoint database files recover step 7 cleanly at cut=end7
// while the old hardcoded cut=32 fails.
func attackCRASH06(ctx context.Context, r recipe.Recipe, hashes []recipe.FileHashes, baseDB, work string) {
	if !trace.Supported {
		report("CRASH-06", "testing corrected 9.2 NORMAL rule when a reader blocks WAL restart after partial PASSIVE checkpoint",
			"skipped on non-Linux", "LINUX-ONLY: strace unavailable on "+runtime.GOOS, true)
		return
	}
	dir := filepath.Join(work, "crash06")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("crash06 mkdir", err)
	}
	defer os.RemoveAll(dir)

	dbp := filepath.Join(dir, "r.db")
	if err := copyFile(baseDB, dbp, -1); err != nil {
		fatal("crash06 copy", err)
	}
	cmd, err := trace.SelfCommand(modePinnedNormal, "-db", dbp)
	if err != nil {
		fatal("crash06 cmd", err)
	}
	res, err := trace.Run(ctx, trace.Options{Command: cmd})
	if err != nil || res.ExitCode != 0 {
		fatal("crash06 run", fmt.Errorf("%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr))
	}
	realDB, err := filepath.EvalSymlinks(dbp)
	if err != nil {
		fatal("crash06 eval", err)
	}
	walSync := trace.SyncsOf(res.Log.Calls, realDB+"-wal")
	dbSync := trace.SyncsOf(res.Log.Calls, realDB)

	// Parse end7, partial, sameGen from child output.
	var end7 int64
	var partial, sameGen bool
	for _, field := range strings.Fields(strings.TrimSpace(res.Stdout)) {
		if v, ok := strings.CutPrefix(field, "end7="); ok {
			end7, _ = strconv.ParseInt(v, 10, 64)
		}
		if v, ok := strings.CutPrefix(field, "partial="); ok {
			partial = v == "true"
		}
		if v, ok := strings.CutPrefix(field, "sameGen="); ok {
			sameGen = v == "true"
		}
	}

	// Recover at 9.2's corrected rule cut (end7) with post- and pre-checkpoint DB,
	// and verify the old byte-32 cut still fails (proving the no-restart branch is exercised).
	postDB := filepath.Join(dir, "cut-post", "r.db")
	preDB := filepath.Join(dir, "cut-pre", "r.db")
	old32DB := filepath.Join(dir, "cut-rule32", "r.db")
	if err := errors.Join(
		copyFile(dbp, postDB, -1), copyFile(dbp+"-wal", postDB+"-wal", end7),
		copyFile(dbp+".before-ckpt", preDB, -1), copyFile(dbp+"-wal", preDB+"-wal", end7),
		copyFile(dbp, old32DB, -1), copyFile(dbp+"-wal", old32DB+"-wal", walfile.HeaderSize),
	); err != nil {
		fatal("crash06 cuts", err)
	}
	integPost, matchPost, errPost := recoverAndMatch(ctx, r, hashes, postDB)
	integPre, matchPre, errPre := recoverAndMatch(ctx, r, hashes, preDB)
	_, matchOld32, errOld32 := recoverAndMatch(ctx, r, hashes, old32DB)
	if errPost != nil || errPre != nil || errOld32 != nil {
		fatal("crash06 recover", errors.Join(errPost, errPre, errOld32))
	}

	held := partial && sameGen &&
		walSync.HasUnsynced && walSync.FirstUnsynced == end7 &&
		dbSync.Syncs == 0 && dbSync.HasUnsynced &&
		integPost == "ok" && matchPost.Found() && matchPost.Step == checkpointAfter &&
		integPre == "ok" && matchPre.Found() && matchPre.Step == checkpointAfter &&
		!matchOld32.Found()
	got := fmt.Sprintf("strace wal FirstUnsynced=%d (end7=%d), dbSyncs=%d (unsynced=%d); cut=end7 recovers post=step %d (integ=%s) & pre=step %d (integ=%s); old cut=32 gives step %d (%s)",
		walSync.FirstUnsynced, end7, dbSync.Syncs, len(dbSync.Unsynced), matchPost.Step, integPost, matchPre.Step, integPre, matchOld32.Step, strings.TrimSpace(res.Stdout))
	report("CRASH-06", "synchronous=NORMAL commits after a reader-pinned partial PASSIVE checkpoint (no WAL restart)",
		"corrected 9.2 NORMAL rule: strace FirstUnsynced==end7, dbSyncs=0 (unsynced partial backfill), both post- and pre-ckpt DBs recover step 7 at cut=end7 (old cut=32 fails)", got, held)
}

// attackCRASH07 tests edge cases of internal/trace.SyncsOf.
func attackCRASH07() {
	const wal = "/tmp/db-wal"
	const mainDB = "/tmp/db"

	calls := []trace.Syscall{
		{Name: "pwrite64", Path: wal, Offset: 0, Length: 32, Result: 32},
		{Name: "fdatasync", Path: wal, Result: 0},                                          // clears unsynced
		{Name: "pwrite64", Path: wal, Offset: 32, Length: 4120, Result: 4120},              // unsynced #1
		{Name: "fsync", Path: mainDB, Result: 0},                                           // different path: must NOT clear wal
		{Name: "fsync", Path: wal, Result: -1, Errno: "EIO"},                               // failed sync: must NOT clear wal
		{Name: "pwrite64", Path: wal, Offset: 16, Length: 16, Result: -1, Errno: "ENOSPC"}, // failed write: ignored
		{Name: "writev", Path: wal, Offset: 4152, Length: 24, Result: 24},                  // unsynced #2
		{Name: "ftruncate", Path: wal, Length: 4096, Result: 0},                            // unsynced #3, offset=4096
		{Name: "fsync", Path: wal, Unfinished: true},                                       // unfinished: noted, must NOT clear
		{Name: "pwrite64", Path: wal, Offset: 0, Length: 8, Unfinished: true},              // unfinished: noted, must NOT lower FirstUnsynced
	}

	rec := trace.SyncsOf(calls, wal)
	// Expected: Syncs=1 (fdatasync), HasUnsynced=true, len(Unsynced)=3 (pwrite64@32, writev@4152, ftruncate@4096),
	// FirstUnsynced=32, UnfinishedSeen=true.
	ok1 := rec.Syncs == 1 && rec.HasUnsynced && len(rec.Unsynced) == 3 && rec.FirstUnsynced == 32 && rec.UnfinishedSeen

	// Also check ftruncate lowering FirstUnsynced below earlier writes.
	calls2 := []trace.Syscall{
		{Name: "pwrite64", Path: wal, Offset: 100, Length: 50, Result: 50},
		{Name: "ftruncate", Path: wal, Length: 0, Result: 0},
	}
	rec2 := trace.SyncsOf(calls2, wal)
	ok2 := rec2.Syncs == 0 && rec2.HasUnsynced && len(rec2.Unsynced) == 2 && rec2.FirstUnsynced == 0

	held := ok1 && ok2
	got := fmt.Sprintf("case1=(syncs=%d unsynced=%d first=%d unfinished=%t) case2=(unsynced=%d first=%d)",
		rec.Syncs, len(rec.Unsynced), rec.FirstUnsynced, rec.UnfinishedSeen, len(rec2.Unsynced), rec2.FirstUnsynced)
	report("CRASH-07", "internal/trace.SyncsOf edge cases (fdatasync, failed fsync/pwrite64, other-path sync, writev, ftruncate, unfinished)",
		"failed/unfinished/other-path calls do not corrupt sync state; fdatasync clears; ftruncate uses new length", got, held)
}

func main() {
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case modeRandomWriter:
			err = runRandomWriterChild(os.Args[2:])
		case modeCkptWriter:
			err = runCkptWriterChild(os.Args[2:])
		case modeRecover:
			err = runRecoverChild(os.Args[2:])
		case modePinnedNormal:
			err = runPinnedNormalChild(os.Args[2:])
		default:
			goto parent
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

parent:
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	work := filepath.Join(root, "_work", "adv-ch09", "crash-power-falsify")
	if err := os.RemoveAll(work); err != nil {
		fatal("clean work", err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		fatal("mkdir work", err)
	}

	r := recipe.Default().WithSteps(lastStep)
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	baseDB := filepath.Join(work, "base", "r.db")
	if err := os.MkdirAll(filepath.Dir(baseDB), 0o755); err != nil {
		fatal("mkdir base", err)
	}
	if err := r.Build(ctx, baseDB, baseStep); err != nil {
		fatal("build base", err)
	}

	attackCRASH01(ctx, self, r, hashes, baseDB, work)
	attackCRASH02(ctx, r, hashes, baseDB, work)
	attackCRASH03(ctx, self, r, hashes, baseDB, work)
	attackCRASH04(ctx, self, r, hashes, baseDB, work)
	attackCRASH05(ctx, self, r, hashes, baseDB, work)
	attackCRASH06(ctx, r, hashes, baseDB, work)
	attackCRASH07()
}
