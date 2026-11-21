package main

import (
	"bufio"
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
	"runtime/debug"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/procstat"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	uncountedPerConnection = 256 * 1024
	memoryConstant         = 32 << 20
	readQuery              = "SELECT count(*), coalesce(sum(balance), 0), (SELECT user_version FROM pragma_user_version) FROM accounts"
)

var (
	failures      []string
	databases     = flag.Int("dbs", 1000, "database files")
	capacity      = flag.Int("k", 64, "connections the bounded manager keeps open")
	operations    = flag.Int("ops", 4000, "operations in the seeded stream")
	seed          = flag.Uint64("seed", 1, "seed of the operation stream")
	maxStep       = flag.Int("max-step", 4, "last recipe step a write may apply")
	writePercent  = flag.Int("write-percent", 20, "share of operations that apply the next recipe step")
	cacheSize     = flag.Int("cache-size", -256, "PRAGMA cache_size of every managed connection")
	sampleEvery   = flag.Int("sample-every", 10, "operations between descriptor samples")
	memoryEvery   = flag.Int("memory-every", 100, "operations between process-memory samples")
	hotPercent    = flag.Int("hot-percent", 50, "share of operations that go to the hot 5% of databases")
	modeFlag      = flag.String("mode", "", "internal: run one stream in this process (bounded or unbounded)")
	runDirFlag    = flag.String("dir", "", "internal: directory of the stream's database files")
	childRoleFlag = flag.String("role", "", "internal: child role for part B")
)

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

var stepHashes []recipe.FileHashes

func matchStep(ctx context.Context, path string) (recipe.Match, error) {
	if stepHashes == nil {
		hashes, err := theRecipe().StepHashes(ctx)
		if err != nil {
			return recipe.Match{Step: -1}, err
		}
		stepHashes = hashes
	}
	if err := recipe.CheckAtRest(path); err != nil {
		return recipe.Match{Step: -1}, err
	}
	image, err := os.ReadFile(path)
	if err != nil {
		return recipe.Match{Step: -1}, err
	}
	match := recipe.MatchHashes(stepHashes, recipe.HashImage(image))
	if match.Found() && !match.Exact && !recipe.ChangeCountersAgree(image) {
		return recipe.Match{Step: -1}, nil
	}
	return match, nil
}

func theRecipe() recipe.Recipe { return recipe.Default().WithSteps(*maxStep) }

func openOptions() sqlitex.Options {
	options := theRecipe().SQLiteOptions()
	options.Conn.CacheSize = *cacheSize
	return options
}

func openManaged(ctx context.Context, path string) (*sqlitex.DB, error) {
	return sqlitex.Open(ctx, path, openOptions())
}

func cacheBudgetBytes() int64 {
	if *cacheSize < 0 {
		return int64(-*cacheSize) * 1024
	}
	return int64(*cacheSize) * 4096
}

func dbID(i int) string { return fmt.Sprintf("db-%04d", i) }

func pathFor(dir string) func(string) (string, error) {
	return func(id string) (string, error) {
		var n int
		if _, err := fmt.Sscanf(id, "db-%04d", &n); err != nil || dbID(n) != id || n < 0 || n >= *databases {
			return "", fmt.Errorf("unknown database id %q", id)
		}
		return filepath.Join(dir, id+".db"), nil
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func populate(master, dir string, n int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	for i := range n {
		if err := copyFile(master, filepath.Join(dir, dbID(i)+".db")); err != nil {
			fatal("copy", err)
		}
	}
}

type operation struct {
	db    int
	write bool
}

func stream() []operation {
	rng := rand.New(rand.NewPCG(*seed, 83))
	hot := max(1, *databases/20)
	steps := make([]int, *databases)
	ops := make([]operation, *operations)
	for i := range ops {
		db := rng.IntN(*databases)
		if rng.IntN(100) < *hotPercent {
			db = rng.IntN(hot)
		}
		write := rng.IntN(100) < *writePercent && steps[db] < *maxStep
		if write {
			steps[db]++
		}
		ops[i] = operation{db: db, write: write}
	}
	return ops
}

func descriptorCount() int {
	n, err := procstat.DescriptorCount()
	if err != nil {
		fatal("descriptors", err)
	}
	return n
}

func settledRSS() int64 {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := procstat.ReadMemory()
	if err != nil {
		fatal("memory", err)
	}
	return m.RSS
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func runStream(ctx context.Context, mode, dir string) {
	limit := *capacity
	if mode == "unbounded" {
		limit = *databases
	}
	var evictedAtRest, evictedNotAtRest int
	var notAtRestReasons []string
	manager, err := connmgr.New(connmgr.Options{
		Capacity: limit,
		PathFor:  pathFor(dir),
		Open:     openManaged,
		OnEvict: func(e connmgr.Eviction) {
			if e.CloseErr != nil || e.StatusErr != nil {
				fatal("evict "+e.ID, errors.Join(e.CloseErr, e.StatusErr))
			}
			if e.Status.AtRest {
				evictedAtRest++
			} else {
				evictedNotAtRest++
				notAtRestReasons = append(notAtRestReasons, e.ID+": "+e.Status.String())
			}
		},
	})
	if err != nil {
		fatal("manager", err)
	}
	r := theRecipe()
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	model := make([]int, *databases)
	baseFDs := descriptorCount()
	baseRSS := settledRSS()
	maxFDs, maxRSS, fdSamples, fdMismatches, readMismatches, writes, reads := 0, int64(0), 0, 0, 0, 0, 0
	var warm, cold time.Duration
	var warmCount, coldCount int
	sampleFDs := func() {
		fds := descriptorCount() - baseFDs
		fdSamples++
		maxFDs = max(maxFDs, fds)
		if fds != 3*manager.Stats().Open {
			fdMismatches++
		}
	}
	sampleRSS := func() {
		m, err := procstat.ReadMemory()
		if err != nil {
			fatal("memory", err)
		}
		maxRSS = max(maxRSS, m.RSS-baseRSS)
	}
	started := time.Now()
	for i, op := range stream() {
		id := dbID(op.db)
		missesBefore := manager.Stats().Misses
		began := time.Now()
		h, err := manager.Acquire(ctx, id)
		if err != nil {
			fatal("acquire "+id, err)
		}
		if op.write {
			if err := recipe.ApplyStep(ctx, h.DB, plan[model[op.db]+1]); err != nil {
				fatal("write "+id, err)
			}
			model[op.db]++
			writes++
		} else {
			var rows, balance int64
			var version int
			if err := h.DB.QueryRowContext(ctx, readQuery).Scan(&rows, &balance, &version); err != nil {
				fatal("read "+id, err)
			}
			if version != model[op.db] {
				readMismatches++
			}
			reads++
			if manager.Stats().Misses > missesBefore {
				cold += time.Since(began)
				coldCount++
			} else {
				warm += time.Since(began)
				warmCount++
			}
		}
		h.Release()
		if (i+1)%*sampleEvery == 0 {
			sampleFDs()
		}
		if (i+1)%*memoryEvery == 0 {
			sampleRSS()
		}
	}
	sampleFDs()
	sampleRSS()
	elapsed := time.Since(started)
	stats := manager.Stats()
	if err := manager.Close(); err != nil {
		fatal("close manager", err)
	}
	afterClose := descriptorCount() - baseFDs
	exact, masked, wrong := 0, 0, 0
	for i := range *databases {
		match, err := matchStep(ctx, filepath.Join(dir, dbID(i)+".db"))
		if err != nil {
			fatal("match "+dbID(i), err)
		}
		switch {
		case match.Step != model[i]:
			wrong++
		case match.Exact:
			exact++
		default:
			masked++
		}
	}
	fmt.Printf("\n%s run: capacity %d\n", mode, limit)
	fmt.Printf("  operations: %d (%d reads, %d writes); distinct databases touched: %d\n", *operations, reads, writes, stats.DistinctDatabases)
	fmt.Printf("  manager: hits %d, misses (opens) %d, evictions %d (at rest %d, not at rest %d), reopens verified against the recorded sha256 %d, refused %d, first opens %d, peak open %d\n",
		stats.Hits, stats.Misses, evictedAtRest+evictedNotAtRest, evictedAtRest, evictedNotAtRest, stats.VerifiedReopens, stats.RefusedReopens, stats.UnverifiedOpens, stats.MaxOpen)
	fmt.Printf("  descriptors: %d samples, peak %d above the baseline; samples where descriptors != 3 x open connections: %d; after Close: %d\n", fdSamples, maxFDs, fdMismatches, afterClose)
	fmt.Printf("  reads whose user_version disagreed with the model: %d\n", readMismatches)
	fmt.Printf("  final files vs the recipe state the model expects: %d exact bytes, %d equal except the change counter, %d wrong\n", exact, masked, wrong)
	budget := int64(limit)*(2*cacheBudgetBytes()+uncountedPerConnection) + memoryConstant
	fmt.Printf("INFO: %s peak process-memory growth %s; budget %d x (2 x %d KiB cache + %d KiB) + %s = %s\n", mode, mib(maxRSS), limit, cacheBudgetBytes()/1024, uncountedPerConnection/1024, mib(memoryConstant), mib(budget))
	fmt.Printf("INFO: %s mean read latency: warm (connection already open) %s over %d, cold (open + verify + read) %s over %d; stream took %s\n",
		mode, meanOf(warm, warmCount), warmCount, meanOf(cold, coldCount), coldCount, elapsed.Round(time.Millisecond))
	fmt.Printf("RESULT %s peak-fds=%d peak-rss=%d budget=%d warm-ns=%d cold-ns=%d\n", mode, maxFDs, maxRSS, budget, nanos(warm, warmCount), nanos(cold, coldCount))
	for _, reason := range notAtRestReasons {
		fmt.Printf("  not at rest at eviction: %s\n", reason)
	}
	check(fdMismatches == 0 && maxFDs == 3*stats.MaxOpen && afterClose == 0, fmt.Sprintf("%s: at every sample the process held exactly 3 descriptors per open connection, at most %d; none after Close", mode, 3*stats.MaxOpen))
	check(stats.MaxOpen <= limit, fmt.Sprintf("%s: never more than %d connections open", mode, limit))
	check(evictedNotAtRest == 0 && stats.RefusedReopens == 0 && stats.VerifiedReopens == stats.Misses-stats.UnverifiedOpens, fmt.Sprintf("%s: every eviction left the file at rest, and every reopen matched the sha256 recorded at its eviction", mode))
	check(readMismatches == 0 && wrong == 0, fmt.Sprintf("%s: every read saw the model's step, and every final file equals the recipe state for its step", mode))
	if mode == "bounded" {
		check(maxRSS <= budget, fmt.Sprintf("bounded: peak process-memory growth stayed within K x (2 x cache budget + %d KiB) + %d MiB", uncountedPerConnection/1024, memoryConstant>>20))
	}
	if len(failures) > 0 {
		os.Exit(1)
	}
}

func meanOf(total time.Duration, n int) time.Duration {
	if n == 0 {
		return 0
	}
	return (total / time.Duration(n)).Round(time.Microsecond)
}

func nanos(total time.Duration, n int) int64 {
	if n == 0 {
		return 0
	}
	return int64(total) / int64(n)
}

type runResult struct {
	peakFDs, peakRSS, budget, warm, cold int64
}

func runChild(ctx context.Context, mode, dir string) runResult {
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	args := []string{"-mode", mode, "-dir", dir}
	flag.Visit(func(f *flag.Flag) {
		if f.Name != "mode" && f.Name != "dir" && f.Name != "role" {
			args = append(args, "-"+f.Name, f.Value.String())
		}
	})
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fatal("pipe", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start "+mode, err)
	}
	var result runResult
	found := false
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "RESULT ") {
			if _, err := fmt.Sscanf(line, "RESULT "+mode+" peak-fds=%d peak-rss=%d budget=%d warm-ns=%d cold-ns=%d", &result.peakFDs, &result.peakRSS, &result.budget, &result.warm, &result.cold); err != nil {
				fatal("parse "+line, err)
			}
			found = true
			fmt.Println("INFO: " + line)
			continue
		}
		fmt.Println(line)
	}
	if err := scanner.Err(); err != nil {
		fatal("read child", err)
	}
	if err := cmd.Wait(); err != nil {
		failures = append(failures, mode+" run: "+err.Error())
		fmt.Printf("check FAILED: the %s run exited with %v\n", mode, err)
	}
	if !found {
		fatal(mode, errors.New("child printed no RESULT line"))
	}
	return result
}

func partA(ctx context.Context, work, master string) {
	soft, hard, err := procstat.FileLimit()
	if err != nil {
		fatal("limit", err)
	}
	fmt.Printf("\n== A. %d databases, a seeded stream of %d operations, LRU manager with K = %d ==\n", *databases, *operations, *capacity)
	fmt.Printf("INFO: descriptor limit: soft %d, hard %d; the unbounded run needs about 3 x %d\n", soft, hard, *databases)
	if uint64(3**databases+64) > soft {
		fatal("descriptor limit", fmt.Errorf("soft limit %d is below the %d descriptors the unbounded run may need; lower -dbs", soft, 3**databases+64))
	}
	fmt.Printf("every database: a copy of recipe step 0 (sha256 %s); a write applies the database's next recipe step (up to %d); a read runs: %s\n", fileHash(master), *maxStep, readQuery)
	fmt.Printf("stream: seed %d, %d%% of operations to the hot %d databases, %d%% writes; connections: recipe settings with cache_size=%d\n", *seed, *hotPercent, max(1, *databases/20), *writePercent, *cacheSize)
	fmt.Println("each run happens in its own child process, on its own copies of the files, so descriptor and memory baselines are clean")
	bounded := runChild(ctx, "bounded", filepath.Join(work, "bounded"))
	unbounded := runChild(ctx, "unbounded", filepath.Join(work, "unbounded"))
	fmt.Println("\ncontrast:")
	check(unbounded.peakFDs > 3*int64(*capacity), fmt.Sprintf("the unbounded run's peak descriptors exceed the bounded run's limit 3 x K = %d", 3**capacity))
	check(unbounded.peakRSS > bounded.peakRSS, "the unbounded run's peak process-memory growth exceeds the bounded run's (SHAPE)")
	fmt.Printf("INFO: peak memory growth bounded %s vs unbounded %s; cold read %s vs warm %s (bounded run)\n", mib(bounded.peakRSS), mib(unbounded.peakRSS), time.Duration(bounded.cold), time.Duration(bounded.warm))
}

func fileHash(path string) string {
	status, err := connmgr.CheckAtRest(path)
	if err != nil || !status.AtRest {
		fatal("hash "+path, fmt.Errorf("%v %v", status, err))
	}
	return status.SHA256
}

func walSize(path string) string {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		return "no -wal"
	}
	return fmt.Sprintf("-wal %d bytes", info.Size())
}

func expectStep(ctx context.Context, path string, want int, claim string) {
	match, err := matchStep(ctx, path)
	if err != nil {
		fatal("match "+path, err)
	}
	fmt.Printf("    %s: %s\n", filepath.Base(path), match)
	check(match.Step == want, claim)
}

type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines *bufio.Scanner
}

func startChild(ctx context.Context, role, path string) *child {
	self, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.CommandContext(ctx, self, "-role", role, "-dir", path, "-max-step", fmt.Sprint(*maxStep), "-cache-size", fmt.Sprint(*cacheSize))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("stdin", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("stdout", err)
	}
	if err := cmd.Start(); err != nil {
		fatal("start child", err)
	}
	c := &child{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(stdout)}
	if !c.lines.Scan() || c.lines.Text() != "ready" {
		fatal("child "+role, fmt.Errorf("did not report ready: %q %v", c.lines.Text(), c.lines.Err()))
	}
	return c
}

func childMain(ctx context.Context, role, path string) {
	runtime.LockOSThread()
	db, err := openManaged(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	switch role {
	case "reader":
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
			fatal("child read", err)
		}
	case "writer":
		if _, err := theRecipe().ApplyNext(ctx, db); err != nil {
			fatal("child write", err)
		}
	default:
		fatal("role", fmt.Errorf("unknown role %q", role))
	}
	fmt.Println("ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fatal("child stdin", err)
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func newManager(dir string) *connmgr.Manager {
	m, err := connmgr.New(connmgr.Options{Capacity: 4, PathFor: pathFor(dir), Open: openManaged})
	if err != nil {
		fatal("manager", err)
	}
	return m
}

func writeNext(ctx context.Context, m *connmgr.Manager, id string) *connmgr.Handle {
	h, err := m.Acquire(ctx, id)
	if err != nil {
		fatal("acquire "+id, err)
	}
	if _, err := theRecipe().ApplyNext(ctx, h.DB); err != nil {
		fatal("write "+id, err)
	}
	return h
}

func evict(m *connmgr.Manager, id string) connmgr.Status {
	e, err := m.Evict(id)
	if err != nil {
		fatal("evict "+id, err)
	}
	if e.CloseErr != nil || e.StatusErr != nil {
		fatal("evict "+id, errors.Join(e.CloseErr, e.StatusErr))
	}
	fmt.Printf("    eviction of %s: %s\n", id, e.Status)
	return e.Status
}

func atRest(path string) connmgr.Status {
	status, err := connmgr.CheckAtRest(path)
	if err != nil {
		fatal("check "+path, err)
	}
	return status
}

func partB(ctx context.Context, work, master string) {
	fmt.Println("\n== B. when a closed WAL database is NOT a single file at rest ==")
	dir := filepath.Join(work, "B")
	populate(master, dir, 6)
	m := newManager(dir)
	path := func(i int) string { return filepath.Join(dir, dbID(i)+".db") }

	fmt.Println("B1. another process still has the database open when the manager closes its connection")
	writeNext(ctx, m, dbID(0)).Release()
	reader := startChild(ctx, "reader", path(0))
	status := evict(m, dbID(0))
	check(!status.AtRest && len(status.Problems) == 2, "B1: the manager's close could not checkpoint or delete the WAL (another connection holds a lock), so the file is reported not at rest")
	if err := reader.stdin.Close(); err != nil {
		fatal("close child stdin", err)
	}
	if err := reader.cmd.Wait(); err != nil {
		fatal("reader exit", err)
	}
	fmt.Printf("    after the other process closed: %s\n", atRest(path(0)))
	expectStep(ctx, path(0), 1, "B1: the last connection to close (the other process) checkpointed and deleted the WAL: at rest, holding step 1")

	fmt.Println("B2. a statement left open (rows not closed) when the handle is released")
	h := writeNext(ctx, m, dbID(1))
	rows, err := h.DB.QueryContext(ctx, "SELECT id FROM accounts ORDER BY id")
	if err != nil {
		fatal("query", err)
	}
	if !rows.Next() {
		fatal("rows", rows.Err())
	}
	h.Release()
	status = evict(m, dbID(1))
	check(!status.AtRest, "B2: database/sql cannot close a connection that a statement still uses, so DB.Close returned and the file is not at rest")
	if err := rows.Close(); err != nil {
		fatal("rows close", err)
	}
	fmt.Printf("    after rows.Close: %s\n", atRest(path(1)))
	expectStep(ctx, path(1), 1, "B2: closing the rows let the connection close: at rest, holding step 1")

	fmt.Println("B3. the process that wrote is killed before it closes")
	writer := startChild(ctx, "writer", path(2))
	if err := writer.cmd.Process.Kill(); err != nil {
		fatal("kill", err)
	}
	if err := writer.cmd.Wait(); err == nil {
		fatal("writer", errors.New("killed child exited cleanly"))
	}
	status = atRest(path(2))
	fmt.Printf("    after the kill: %s\n", status)
	check(!status.AtRest && len(status.Problems) == 2, "B3: a killed writer leaves a -wal with its committed frames and a -shm")
	naive := filepath.Join(dir, "naive-copy.db")
	if err := copyFile(path(2), naive); err != nil {
		fatal("copy", err)
	}
	expectStep(ctx, naive, 0, "B3: a copy of the database file alone is a valid database WITHOUT the committed step 1 (it is still in the -wal)")
	opened, err := openManaged(ctx, path(2))
	if err != nil {
		fatal("reopen", err)
	}
	if err := opened.Close(); err != nil {
		fatal("close", err)
	}
	fmt.Printf("    after one clean open and close: %s\n", atRest(path(2)))
	expectStep(ctx, path(2), 1, "B3: one clean open and close recovers the WAL, checkpoints it and deletes it: at rest, holding step 1")

	fmt.Println("B4. SQLITE_FCNTL_PERSIST_WAL is set on the connection")
	h = writeNext(ctx, m, dbID(3))
	conn, err := h.DB.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	if _, err := sqlitex.PersistWAL(ctx, conn, "main", 1); err != nil {
		fatal("persist wal", err)
	}
	if err := conn.Close(); err != nil {
		fatal("conn close", err)
	}
	h.Release()
	status = evict(m, dbID(3))
	problems := strings.Join(status.Problems, ";")
	check(!status.AtRest && strings.Contains(problems, "-wal holds") && strings.Contains(problems, "-shm exists"), "B4: with PERSIST_WAL the close checkpoints but keeps the -wal (with its bytes) and the -shm, so the manager refuses to call it at rest")
	persisted := filepath.Join(dir, "persist-wal-copy.db")
	if err := copyFile(path(3), persisted); err != nil {
		fatal("copy", err)
	}
	expectStep(ctx, persisted, 1, "B4: the refusal is conservative here: the close did checkpoint, so the database file alone already holds step 1; a rule based on which files exist cannot see that")

	fmt.Println("B5. the file is renamed while its connection is open")
	writeNext(ctx, m, dbID(4)).Release()
	moved := filepath.Join(dir, "moved.db")
	if err := os.Rename(path(4), moved); err != nil {
		fatal("rename", err)
	}
	status = evict(m, dbID(4))
	leftBehind := walSize(path(4))
	fmt.Printf("    left at the old name: %s\n", leftBehind)
	check(!status.AtRest && len(status.Problems) > 0 && status.Problems[0] == "database file missing", "B5: the manager reports the database file missing at its path")
	check(leftBehind != "no -wal" && leftBehind != "-wal 0 bytes", "B5: a -wal with content stays behind at the old name")
	expectStep(ctx, moved, 0, "B5: the moved file looks at rest but holds step 0: close saw the file had moved, skipped the checkpoint, and step 1 stayed in the -wal under the old name")

	fmt.Println("B6. the file changes while it is closed")
	writeNext(ctx, m, dbID(5)).Release()
	recorded := evict(m, dbID(5))
	outside, err := openManaged(ctx, path(5))
	if err != nil {
		fatal("outside open", err)
	}
	if _, err := theRecipe().ApplyNext(ctx, outside); err != nil {
		fatal("outside write", err)
	}
	if err := outside.Close(); err != nil {
		fatal("outside close", err)
	}
	_, err = m.Acquire(ctx, dbID(5))
	var changed *connmgr.ChangedWhileClosedError
	fmt.Printf("    reopen: %v\n", err)
	check(recorded.AtRest && errors.As(err, &changed) && changed.Found.AtRest && changed.Found.SHA256 != recorded.SHA256, "B6: a write made outside the manager while the file was closed changes its sha256, and the manager refuses to reopen it")
	if err := m.Close(); err != nil {
		fatal("close manager", err)
	}
}

func main() {
	flag.Parse()
	ctx := context.Background()
	if *childRoleFlag != "" {
		childMain(ctx, *childRoleFlag, *runDirFlag)
		return
	}
	if *modeFlag != "" {
		runStream(ctx, *modeFlag, *runDirFlag)
		return
	}
	sqlitex.MustPinCheckMemory(ctx)
	if *databases < 20 || *capacity < 1 || *capacity >= *databases || *operations < 1 || *maxStep < 1 || *maxStep > recipe.DefaultSteps || *sampleEvery < 1 || *memoryEvery < 1 || *writePercent < 0 || *writePercent > 100 || *hotPercent < 0 || *hotPercent > 100 || *cacheSize == 0 {
		fatal("flags", fmt.Errorf("need -dbs >= 20, 1 <= -k < -dbs, -ops >= 1, 1 <= -max-step <= %d, positive sample intervals, percentages 0..100, -cache-size != 0", recipe.DefaultSteps))
	}
	work := sqlitex.MustWorkDir("8.3")
	master := filepath.Join(work, "master.db")
	if err := theRecipe().Build(ctx, master, 0); err != nil {
		fatal("build master", err)
	}
	populate(master, filepath.Join(work, "bounded"), *databases)
	populate(master, filepath.Join(work, "unbounded"), *databases)
	partA(ctx, work, master)
	partB(ctx, work, master)
	if len(failures) > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
