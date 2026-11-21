// Experiment 4.2 measures SQLite's page-cache memory budget and process RSS
// across five scenarios in WAL mode: one connection scanning twice at two
// cache_size settings (Part A), N open connections to N files (Part B), N open
// connections to one file (Part C), N files visited through a K-connection LRU
// pool (Part D), and memory-mapped reads via PRAGMA mmap_size (Part E).
//
// Run from the repo root (portable; the anonymous vs file-backed RSS split in
// Part E uses /proc/self/status on Linux):
//
//	go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize  = 4096
	bodyBytes = 3500
	scanSQL   = "SELECT count(*), sum(length(body)) FROM t"
)

var (
	failures      []string
	connectionsN  = flag.Int("n", 64, "connections opened in the unbounded runs")
	boundK        = flag.Int("k", 8, "open connections allowed by the bounded LRU pool")
	cacheSizeFlag = flag.Int("cache-size", -2000, "PRAGMA cache_size for every connection (negative = KiB, positive = pages)")
	rowsFlag      = flag.Int("rows", 1000, "rows per database file; one row of 3500 bytes fills one 4096-byte leaf page")
)

type processMemory struct {
	RSS   int64
	Anon  int64
	File  int64
	Shmem int64
}

type connStat struct {
	CacheUsed int
	Hits      int
	Misses    int
	Spills    int
}

type checkpoint struct {
	Label   string
	Open    int
	Visited int
	Conns   []connStat
	Memory  processMemory
}

var perPage = 1

// cachedPages derives the cached page count from SQLITE_DBSTATUS_CACHE_USED
// (sqlite3PagerMemUsed, src/pager.c:6881-6887). Subtracting the single
// page-sized buffer and dividing by perPage discards the Pager struct's
// path-length-dependent allocation (as long as it stays below perPage), making
// page counts identical across host and container paths.
func cachedPages(cacheUsed int) int {
	return max(0, (cacheUsed-pageSize)/perPage)
}

func (c checkpoint) pagesTotal() int {
	total := 0
	for _, s := range c.Conns {
		total += cachedPages(s.CacheUsed)
	}
	return total
}

func (c checkpoint) pagesMinMax() (int, int) {
	lo, hi := c.cacheMinMax()
	return cachedPages(lo), cachedPages(hi)
}

func (c checkpoint) cacheTotal() int {
	total := 0
	for _, s := range c.Conns {
		total += s.CacheUsed
	}
	return total
}

func (c checkpoint) cacheMinMax() (int, int) {
	if len(c.Conns) == 0 {
		return 0, 0
	}
	lo, hi := c.Conns[0].CacheUsed, c.Conns[0].CacheUsed
	for _, s := range c.Conns {
		lo, hi = min(lo, s.CacheUsed), max(hi, s.CacheUsed)
	}
	return lo, hi
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
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func options(cacheSize int) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.SingleConnection = true
	o.Conn.CacheSize = cacheSize
	return o
}

// budgetBytes converts PRAGMA cache_size to bytes matching numberOfCachePages
// (src/pcache.c:277-292): negative values are KiB, positive values are pages.
func budgetBytes(cacheSize int) int {
	if cacheSize < 0 {
		return -1024 * cacheSize
	}
	return cacheSize * pageSize
}

// Flag floors ensuring calibration and RSS checks stay meaningful: Part A
// calibrates bytes per page from cache_size=200 minus cache_size=100, and RSS
// checks require at least 1 MiB of cache per connection so page-cache memory
// dominates fixed per-connection overhead and allocator noise.
const (
	calibrationPages = 200
	minBudgetBytes   = 1 << 20
)

// minimumRows returns the smallest row count (1.2x the larger of the 200-page
// calibration cache and the requested cache budget) so the database exceeds
// both caches and misses on a full scan.
func minimumRows(cacheSize int) int {
	return max(calibrationPages, budgetBytes(cacheSize)/pageSize) * 6 / 5
}

// withinBudget checks Part A's budget claim: positive PRAGMA cache_size counts
// pages directly (numberOfCachePages, src/pcache.c:277-292), while negative
// cache_size is a KiB byte budget that already accounts for pageSize + szExtra.
func withinBudget(cacheUsed, cacheSize int) bool {
	if cacheSize > 0 {
		return cachedPages(cacheUsed) <= cacheSize
	}
	return cacheUsed <= budgetBytes(cacheSize)*105/100
}

func fileName(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("f-%03d.db", i))
}

// buildSource creates one deterministic WAL-mode database with rows of 3500
// PRNG bytes each so every row fills a single 4096-byte leaf page, giving a
// predictable multi-megabyte table that exceeds the default -2000 KiB cache.
func buildSource(ctx context.Context, path string, rows int) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, path, options(-2000))
	if err != nil {
		fatal("create source", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create table", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	rng := rand.New(rand.NewPCG(4, 2))
	body := make([]byte, bodyBytes)
	for id := 1; id <= rows; id++ {
		for i := range body {
			body[i] = byte(rng.Uint32())
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, body) VALUES(?, ?)", id, body); err != nil {
			fatal("insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

type handle struct {
	db   *sqlitex.DB
	conn *sql.Conn
}

func openHandle(ctx context.Context, path string, cacheSize int) handle {
	db, err := sqlitex.Open(ctx, path, options(cacheSize))
	if err != nil {
		fatal("open", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	return handle{db: db, conn: conn}
}

func (h handle) close() {
	if err := h.conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := h.db.Close(); err != nil {
		fatal("close db", err)
	}
}

func (h handle) scan(ctx context.Context) {
	var count, total int64
	if err := h.conn.QueryRowContext(ctx, scanSQL).Scan(&count, &total); err != nil {
		fatal("scan", err)
	}
}

func (h handle) stat(ctx context.Context) connStat {
	read := func(op sqlitex.DBStatusOp) int {
		s, err := sqlitex.ReadDBStatus(ctx, h.conn, op, false)
		if err != nil {
			fatal("db_status", err)
		}
		return s.Current
	}
	return connStat{CacheUsed: read(sqlitex.DBStatusCacheUsed), Hits: read(sqlitex.DBStatusCacheHit), Misses: read(sqlitex.DBStatusCacheMiss), Spills: read(sqlitex.DBStatusCacheSpill)}
}

// measure forces a Go GC and returns unused Go spans to the OS before sampling
// process memory so RSS deltas reflect SQLite's page cache and file mappings.
func measure(label string, open, visited int, stats []connStat) {
	runtime.GC()
	debug.FreeOSMemory()
	m, err := readProcessMemory()
	if err != nil {
		fatal("process memory", err)
	}
	line, err := json.Marshal(checkpoint{Label: label, Open: open, Visited: visited, Conns: stats, Memory: m})
	if err != nil {
		fatal("json", err)
	}
	fmt.Println(string(line))
}

func reportPoints(n int) []int {
	points := []int{1}
	for p := 2; p < n; p *= 2 {
		points = append(points, p)
	}
	if n > 1 {
		points = append(points, n)
	}
	return points
}

func childUnbounded(ctx context.Context, dir string, n, cacheSize int, oneFile bool) {
	measure("start", 0, 0, nil)
	var handles []handle
	points := reportPoints(n)
	for i := range n {
		path := fileName(dir, i)
		if oneFile {
			path = fileName(dir, 0)
		}
		h := openHandle(ctx, path, cacheSize)
		h.scan(ctx)
		handles = append(handles, h)
		if slices.Contains(points, i+1) {
			stats := make([]connStat, len(handles))
			for j, other := range handles {
				stats[j] = other.stat(ctx)
			}
			measure(fmt.Sprintf("after %d", i+1), len(handles), i+1, stats)
		}
	}
	for _, h := range handles {
		h.close()
	}
	measure("all closed", 0, n, nil)
}

func childBounded(ctx context.Context, dir string, n, k, cacheSize int) {
	measure("start", 0, 0, nil)
	var pool []handle
	points := reportPoints(n)
	for i := range n {
		if len(pool) == k {
			pool[0].close()
			pool = pool[1:]
		}
		h := openHandle(ctx, fileName(dir, i), cacheSize)
		h.scan(ctx)
		pool = append(pool, h)
		if slices.Contains(points, i+1) || i+1 == k {
			stats := make([]connStat, len(pool))
			for j, other := range pool {
				stats[j] = other.stat(ctx)
			}
			measure(fmt.Sprintf("after %d", i+1), len(pool), i+1, stats)
		}
	}
	for _, h := range pool {
		h.close()
	}
}

func childScans(ctx context.Context, dir string, cacheSize int) {
	h := openHandle(ctx, fileName(dir, 0), cacheSize)
	defer h.close()
	var stats []connStat
	for range 2 {
		h.scan(ctx)
		stats = append(stats, h.stat(ctx))
	}
	measure("scans", 1, 1, stats)
}

func childMmap(ctx context.Context, dir string, cacheSize int, mmapSize int64) {
	measure("start", 0, 0, nil)
	plain := openHandle(ctx, fileName(dir, 0), cacheSize)
	plain.scan(ctx)
	measure("plain", 1, 1, []connStat{plain.stat(ctx)})
	mapped := openHandle(ctx, fileName(dir, 1), cacheSize)
	var got int64
	if err := mapped.conn.QueryRowContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", mmapSize)).Scan(&got); err != nil {
		fatal("mmap_size", err)
	}
	if got != mmapSize {
		fatal("mmap_size", fmt.Errorf("PRAGMA mmap_size returned %d, want %d", got, mmapSize))
	}
	mapped.scan(ctx)
	measure("mapped", 2, 2, []connStat{plain.stat(ctx), mapped.stat(ctx)})
	mapped.close()
	plain.close()
}

func childMain(args []string) {
	ctx := context.Background()
	atoi := func(s string) int {
		v, err := strconv.Atoi(s)
		if err != nil {
			fatal("child args", err)
		}
		return v
	}
	switch args[0] {
	case "files":
		childUnbounded(ctx, args[1], atoi(args[2]), atoi(args[3]), false)
	case "one-file":
		childUnbounded(ctx, args[1], atoi(args[2]), atoi(args[3]), true)
	case "bounded":
		childBounded(ctx, args[1], atoi(args[2]), atoi(args[3]), atoi(args[4]))
	case "scans":
		childScans(ctx, args[1], atoi(args[2]))
	case "mmap":
		childMmap(ctx, args[1], atoi(args[2]), int64(atoi(args[3])))
	default:
		fatal("child", fmt.Errorf("unknown role %q", args[0]))
	}
}

// runChild executes one measurement scenario in a fresh child process so every
// scenario starts from a clean RSS baseline and a fresh pcache1 PGroup.
func runChild(args ...string) []checkpoint {
	exe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.Command(exe, append([]string{"child"}, args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal("child "+args[0], err)
	}
	var points []checkpoint
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var c checkpoint
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			fatal("child output", fmt.Errorf("%w: %q", err, line))
		}
		points = append(points, c)
	}
	return points
}

func mib(b int64) string {
	if b < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

func memoryInfo(m processMemory) string {
	if m.Anon < 0 {
		return "RSS " + mib(m.RSS)
	}
	return fmt.Sprintf("RSS %s (anon %s, file %s, shmem %s)", mib(m.RSS), mib(m.Anon), mib(m.File), mib(m.Shmem))
}

func find(points []checkpoint, label string) checkpoint {
	for _, p := range points {
		if p.Label == label {
			return p
		}
	}
	fatal("checkpoint", fmt.Errorf("no checkpoint %q", label))
	return checkpoint{}
}

func printGrowth(points []checkpoint) {
	start := points[0]
	fmt.Printf("  %-12s %5s %18s %32s\n", "checkpoint", "open", "cached pages (sum)", "cached pages per connection min..max")
	for _, p := range points[1:] {
		lo, hi := p.pagesMinMax()
		fmt.Printf("  %-12s %5d %18d %21d..%-10d\n", p.Label, p.Open, p.pagesTotal(), lo, hi)
		fmt.Printf("INFO:   %-12s sum CACHE_USED %d bytes; %s, growth since start %s\n", p.Label, p.cacheTotal(), memoryInfo(p.Memory), mib(p.Memory.RSS-start.Memory.RSS))
	}
}

func lastWithOpen(points []checkpoint) checkpoint {
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].Open > 0 {
			return points[i]
		}
	}
	fatal("checkpoint", errors.New("no checkpoint with open connections"))
	return checkpoint{}
}

func sumsMatch(points []checkpoint, perConnection int) bool {
	for _, p := range points {
		if p.pagesTotal() != p.Open*perConnection {
			return false
		}
	}
	return true
}

// pagesMatch compares path-length-independent cached-page totals across every
// checkpoint (raw CACHE_USED bytes vary with filename length via
// sqlite3MallocSize(pPager), src/pager.c:6881-6887).
func pagesMatch(a, b []checkpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Label != b[i].Label || a[i].pagesTotal() != b[i].pagesTotal() {
			return false
		}
	}
	return true
}

func growth(points []checkpoint, label string) int64 {
	return find(points, label).Memory.RSS - points[0].Memory.RSS
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "child" {
		childMain(os.Args[2:])
		return
	}
	flag.Parse()
	ctx := context.Background()
	started := time.Now()
	sqlitex.MustPinCheckMemory(ctx)
	if *connectionsN < 2 || *boundK < 1 || *boundK >= *connectionsN || *cacheSizeFlag == 0 {
		fatal("flags", errors.New("need n >= 2, 1 <= k < n, cache-size != 0"))
	}
	if minRows := minimumRows(*cacheSizeFlag); *rowsFlag < minRows {
		fatal("flags", fmt.Errorf("-rows %d is too small for -cache-size %d: need rows >= %d so the file is larger than both the cache_size=200 calibration cache and the requested budget", *rowsFlag, *cacheSizeFlag, minRows))
	}
	if budgetBytes(*cacheSizeFlag) < minBudgetBytes {
		fatal("flags", fmt.Errorf("-cache-size %d is a budget of %d bytes; need at least %d so that cache memory dominates per-connection memory in the RSS checks", *cacheSizeFlag, budgetBytes(*cacheSizeFlag), minBudgetBytes))
	}
	work := sqlitex.MustWorkDir("4.2")
	n, k, cacheSize := *connectionsN, *boundK, *cacheSizeFlag
	budget := budgetBytes(cacheSize)
	source := fileName(work, 0)
	buildSource(ctx, source, *rowsFlag)
	info, err := os.Stat(source)
	if err != nil {
		fatal("stat", err)
	}
	for i := 1; i < n; i++ {
		data, err := os.ReadFile(source)
		if err != nil {
			fatal("read source", err)
		}
		if err := os.WriteFile(fileName(work, i), data, 0o644); err != nil {
			fatal("copy", err)
		}
	}
	fmt.Printf("\n== setup ==\n")
	fmt.Printf("database files: %d copies of one WAL-mode file, table t with %d rows of %d bytes, %d pages (%d bytes)\n", n, *rowsFlag, bodyBytes, info.Size()/pageSize, info.Size())
	fmt.Printf("every connection: sqlitex defaults (journal_mode=wal, synchronous=FULL) with cache_size=%d; budget %d bytes per connection\n", cacheSize, budget)
	fmt.Printf("scan: %s (reads every leaf page)\n", scanSQL)
	fmt.Printf("N=%d unbounded connections, K=%d in the bounded pool\n", n, k)
	fmt.Printf("INFO: process memory read from %s; runs in child processes so every measurement starts from a fresh process\n", memorySource)
	var compileMM int
	db, err := sqlitex.OpenMemory(ctx)
	if err != nil {
		fatal("open memory", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT sqlite_compileoption_used('ENABLE_MEMORY_MANAGEMENT')").Scan(&compileMM); err != nil {
		fatal("compile option", err)
	}
	db.Close()
	fmt.Printf("driver compiled with ENABLE_MEMORY_MANAGEMENT: %d (1 = every connection's page cache is one process-wide LRU group, pcache1.c:718-719)\n", compileMM)

	fmt.Printf("\n== A. one connection, the same scan twice ==\n")
	// Measure exact bytes per cached page from sqlite3PagerMemUsed
	// (src/pager.c:6881-6887) by comparing two full caches 100 pages apart.
	// pcache1FetchStage2 recycles at nPage+1 >= nMax (src/pcache1.c:902), so
	// cache_size=100 holds 99 pages and cache_size=200 holds 199 pages; their
	// difference is still exactly 100 pages and cancels the fixed Pager overhead.
	small := runChild("scans", work, "100")[0].Conns[0].CacheUsed
	large := runChild("scans", work, "200")[0].Conns[0].CacheUsed
	if large <= small || (large-small)%100 != 0 {
		fatal("per-page size", fmt.Errorf("CACHE_USED at cache_size=200 minus at 100 is %d, not a multiple of 100", large-small))
	}
	perPage = (large - small) / 100
	fmt.Printf("CACHE_USED counts %d bytes per cached page (measured: cache_size=200 minus cache_size=100, both full, divided by 100); the %d-byte page plus %d bytes of headers (sqlite3PagerMemUsed, pager.c:6881)\n", perPage, pageSize, perPage-pageSize)
	fmt.Printf("INFO: fixed part of CACHE_USED per connection %d bytes (the Pager allocation, which holds the file names, plus one page buffer)\n", small-cachedPages(small)*perPage)
	fmt.Printf("cache_size=100 holds %d pages, cache_size=200 holds %d pages\n", cachedPages(small), cachedPages(large))
	for _, size := range []int{cacheSize, -2 * int(info.Size()) / 1024} {
		scans := runChild("scans", work, strconv.Itoa(size))[0]
		fmt.Printf("cache_size=%d (budget %d bytes):\n", size, budgetBytes(size))
		for i, s := range scans.Conns {
			fmt.Printf("  after scan %d: cached pages=%d hit=%d miss=%d spill=%d\n", i+1, cachedPages(s.CacheUsed), s.Hits, s.Misses, s.Spills)
			fmt.Printf("INFO:   after scan %d: CACHE_USED=%d bytes\n", i+1, s.CacheUsed)
		}
		first, second := scans.Conns[0], scans.Conns[1]
		budgetClaim := fmt.Sprintf("cache_size=%d: CACHE_USED stays within the budget plus 5%% (%d pages x %d bytes against %d bytes)", size, cachedPages(first.CacheUsed), perPage, budgetBytes(size))
		if size > 0 {
			budgetClaim = fmt.Sprintf("cache_size=%d: the cache holds at most %d pages (it holds %d)", size, size, cachedPages(first.CacheUsed))
		}
		check(withinBudget(first.CacheUsed, size), budgetClaim)
		if size == cacheSize {
			check(second.Misses-first.Misses >= int(info.Size()/pageSize)*9/10, "a file larger than the cache misses again on the second scan (the LRU evicted the pages the scan needs next)")
		} else {
			check(second.Misses == first.Misses && second.Hits > first.Hits, "a cache larger than the file serves the second scan from memory: no new misses")
		}
	}

	full := cachedPages(runChild("scans", work, strconv.Itoa(cacheSize))[0].Conns[0].CacheUsed)
	nLabel, halfLabel := fmt.Sprintf("after %d", n), fmt.Sprintf("after %d", reportPoints(n)[len(reportPoints(n))-2])

	fmt.Printf("\n== B. %d connections to %d files, all kept open ==\n", n, n)
	files := runChild("files", work, strconv.Itoa(n), strconv.Itoa(cacheSize))
	printGrowth(files)
	last := lastWithOpen(files)
	fmt.Printf("  at %s: connection 1 cached pages=%d hit=%d miss=%d; connection %d cached pages=%d hit=%d miss=%d\n", last.Label, cachedPages(last.Conns[0].CacheUsed), last.Conns[0].Hits, last.Conns[0].Misses, len(last.Conns), cachedPages(last.Conns[len(last.Conns)-1].CacheUsed), last.Conns[len(last.Conns)-1].Hits, last.Conns[len(last.Conns)-1].Misses)
	lo, hi := last.pagesMinMax()
	check(sumsMatch(files, full), fmt.Sprintf("the cached pages of all open connections add up to exactly (open connections) x %d, the pages one connection holds when it fills its budget", full))
	// With ENABLE_MEMORY_MANAGEMENT, pcache1Init sets separateCache=0
	// (src/pcache1.c:718-719), putting every open connection in one process-wide
	// PGroup LRU. Active scanners recycle idle connections' pages
	// (pcache1FetchStage2, src/pcache1.c:899-912) while the group total equals
	// open * perConnBudget (pcache1Cachesize, src/pcache1.c:814-832).
	if compileMM == 1 {
		check(hi > full && lo < full, fmt.Sprintf("in this driver build cache_size is not a per-connection cap: the connection that scanned last holds %d pages (its own budget is %d) while the idle connections were left with %d", hi, full, lo))
	} else {
		check(hi <= full, "every connection stays within its own budget")
	}
	rssN, rssHalf, cacheN := growth(files, nLabel), growth(files, halfLabel), int64(find(files, nLabel).cacheTotal())
	fmt.Printf("INFO: RSS growth at %s %s, at %s %s; RSS growth / summed CACHE_USED %.2f\n", nLabel, mib(rssN), halfLabel, mib(rssHalf), float64(rssN)/float64(cacheN))
	check(rssN >= cacheN*9/10, "process memory grows by at least the summed CACHE_USED of the open connections")
	half := find(files, halfLabel).Open
	perN, perHalf := float64(rssN)/float64(n), float64(rssHalf)/float64(half)
	fmt.Printf("INFO: RSS growth per open connection: %s at %d, %s at %d\n", mib(int64(perN)), n, mib(int64(perHalf)), half)
	check(perN >= perHalf*0.8 && perN <= perHalf*1.25, fmt.Sprintf("process memory grows linearly with open connections: growth per open connection at %d is within -20%%/+25%% of that at %d", n, half))

	fmt.Printf("\n== C. %d connections to one file, all kept open ==\n", n)
	oneFile := runChild("one-file", work, strconv.Itoa(n), strconv.Itoa(cacheSize))
	printGrowth(oneFile)
	check(sumsMatch(oneFile, full) && pagesMatch(oneFile, files), "connections to one file do not share a cache: the totals equal the N-files run at every checkpoint")
	oneN := growth(oneFile, nLabel)
	check(oneN >= cacheN*9/10, "process memory grows by the summed CACHE_USED here too")

	fmt.Printf("\n== D. %d files visited through a pool of at most %d open connections (LRU close) ==\n", n, k)
	bounded := runChild("bounded", work, strconv.Itoa(n), strconv.Itoa(k), strconv.Itoa(cacheSize))
	printGrowth(bounded)
	kLabel := fmt.Sprintf("after %d", k)
	atK, atN := growth(bounded, kLabel), growth(bounded, nLabel)
	fmt.Printf("INFO: RSS growth at %s %s, at %s %s\n", kLabel, mib(atK), nLabel, mib(atN))
	check(find(bounded, nLabel).Open == k && find(bounded, nLabel).pagesTotal() <= k*(full+1), fmt.Sprintf("with the pool, %d connections are open at the end and their cached pages stay within %d x (%d + 1), the sum of their cache sizes in pages", k, k, full))
	check(atN-atK <= atK/4, fmt.Sprintf("with the pool, process memory stays flat after %d connections: growth from %s to %s is at most a quarter of the growth up to %s", k, kLabel, nLabel, kLabel))
	check(float64(atN) <= float64(rssN)*float64(k)/float64(n)*1.5, fmt.Sprintf("the pooled run ends with about %d/%d of the unbounded run's memory growth (at most 1.5 x that fraction)", k, n))

	// PRAGMA mmap_size activates getPageMMap (pagerFixMaplimit,
	// src/pager.c:3579-3590; getPageMMap, src/pager.c:5687-5752), which serves
	// pages directly from the kernel's mapped file cache via sqlite3OsFetch and
	// pagerAcquireMapPage outside pcache1, so CACHE_USED stays near zero while
	// RssFile grows by the file size.
	fmt.Printf("\n== E. mmap ==\n")
	mmapSize := 2 * info.Size()
	fmt.Printf("connection 1: no mmap, scans f-000; connection 2: PRAGMA mmap_size=%d (twice the file), scans f-001\n", mmapSize)
	mapped := runChild("mmap", work, strconv.Itoa(cacheSize), strconv.FormatInt(mmapSize, 10))
	for _, p := range mapped[1:] {
		for i, s := range p.Conns {
			fmt.Printf("  %-7s connection %d: cached pages=%d hit=%d miss=%d\n", p.Label, i+1, cachedPages(s.CacheUsed), s.Hits, s.Misses)
			fmt.Printf("INFO:   %-7s connection %d: CACHE_USED=%d bytes\n", p.Label, i+1, s.CacheUsed)
		}
		fmt.Printf("INFO:   %-7s %s\n", p.Label, memoryInfo(p.Memory))
	}
	plain, withMap := find(mapped, "plain"), find(mapped, "mapped")
	mappedConn := withMap.Conns[1]
	check(mappedConn.CacheUsed*20 < plain.Conns[0].CacheUsed, "with mmap covering the file, the scan leaves CACHE_USED near zero (under 5% of the unmapped connection's)")
	rssDelta := withMap.Memory.RSS - plain.Memory.RSS
	fmt.Printf("INFO: the mapped scan added %s RSS (file %s)\n", mib(rssDelta), mib(info.Size()))
	check(rssDelta >= info.Size()*8/10, "the mapped scan still adds about the file's size to process RSS: the memory moved from SQLite's heap to mapped file pages")
	if withMap.Memory.File >= 0 {
		fileDelta, anonDelta := withMap.Memory.File-plain.Memory.File, withMap.Memory.Anon-plain.Memory.Anon
		fmt.Printf("INFO: file-backed RSS +%s, anonymous RSS +%s\n", mib(fileDelta), mib(anonDelta))
		check(fileDelta >= info.Size()*8/10 && anonDelta < info.Size()/4, "the added memory is file-backed (RssFile), not anonymous heap")
	} else {
		fmt.Println("LINUX-ONLY: the split of RSS into anonymous and file-backed memory (/proc/self/status) skipped on " + runtime.GOOS + "; run it with: docker compose run --rm lab go run ./ch04-pager-and-page-cache/experiments/4.2-cache-budget")
	}

	fmt.Printf("INFO: runtime %s\n", time.Since(started).Round(100*time.Millisecond))
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("CACHE BUDGET FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("CACHE BUDGET OK")
}
