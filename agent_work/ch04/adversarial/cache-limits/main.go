package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize       = 4096
	perPageBytes   = 4352 // 4096 + 256 header bytes measured by 4.2
	scanSQL        = "SELECT count(*), sum(length(body)) FROM t"
	childScanRole  = "child-scan"
	childShrink    = "child-shrink"
	childShrinkPin = "child-shrink-pin"
	childSoftHeap  = "child-soft-heap"
	childMmapWAL   = "child-mmap-wal"
	childPathLen   = "child-path-len"
)

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func workDir() string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	dir := filepath.Join(root, "_work", "adv-ch04", "cache-limits")
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	return dir
}

func readRSS() int64 {
	if runtime.GOOS != "linux" {
		return -1
	}
	runtime.GC()
	debug.FreeOSMemory()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			if kib, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return kib * 1024
			}
		}
	}
	return -1
}

func walOpts(cacheSize int) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.SingleConnection = true
	o.Conn.CacheSize = cacheSize
	return o
}

func buildWALDB(ctx context.Context, path string, rows int) {
	_ = recipe.RemoveDatabase(path)
	db, err := sqlitex.Open(ctx, path, walOpts(-2000))
	if err != nil {
		fatal("create wal db", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create table", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	rng := rand.New(rand.NewPCG(420, uint64(rows)))
	body := make([]byte, 3500)
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
}

func cachedPages(cacheUsed int) int {
	return max(0, (cacheUsed-pageSize)/perPageBytes)
}

type handle struct {
	db   *sqlitex.DB
	conn *sql.Conn
}

func openHandle(ctx context.Context, path string, cacheSize int) handle {
	db, err := sqlitex.Open(ctx, path, walOpts(cacheSize))
	if err != nil {
		fatal("open "+path, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	return handle{db: db, conn: conn}
}

func (h handle) close() {
	_ = h.conn.Close()
	_ = h.db.Close()
}

func (h handle) scan(ctx context.Context) {
	var c, s int64
	if err := h.conn.QueryRowContext(ctx, scanSQL).Scan(&c, &s); err != nil {
		fatal("scan", err)
	}
}

func (h handle) cacheUsed(ctx context.Context) int {
	st, err := sqlitex.ReadDBStatus(ctx, h.conn, sqlitex.DBStatusCacheUsed, false)
	if err != nil {
		fatal("cache_used", err)
	}
	return st.Current
}

func (h handle) statusAll(ctx context.Context) (used, hits, misses, spills int) {
	get := func(op sqlitex.DBStatusOp) int {
		st, err := sqlitex.ReadDBStatus(ctx, h.conn, op, false)
		if err != nil {
			fatal("db_status", err)
		}
		return st.Current
	}
	return get(sqlitex.DBStatusCacheUsed), get(sqlitex.DBStatusCacheHit), get(sqlitex.DBStatusCacheMiss), get(sqlitex.DBStatusCacheSpill)
}

func runSelfJSON(out any, args ...string) {
	exe, err := os.Executable()
	if err != nil {
		fatal("exe", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		fatal("child "+args[0], err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		fatal("unmarshal "+args[0], fmt.Errorf("%w: %q", err, string(raw)))
	}
}

// --- Child implementations ---

type scanOut struct {
	CacheSize int
	CacheUsed int
	Pages     int
	MidPages  int
	Hits      int
	Misses    int
	RSSStart  int64
	RSSEnd    int64
}

func doChildScan(dbPath string, cacheSize int) {
	ctx := context.Background()
	rss0 := readRSS()
	// Open with default cache_size first if cacheSize==0 (sqlitex rejects 0? Let's test via PRAGMA directly!).
	db, err := sqlitex.Open(ctx, dbPath, walOpts(-2000))
	if err != nil {
		fatal("open scan", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn scan", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size=%d", cacheSize)); err != nil {
		fatal("set cache_size", err)
	}
	// First read mid-scan pages while a cursor is actively open on row 10.
	rows, err := conn.QueryContext(ctx, "SELECT id, length(body) FROM t")
	if err != nil {
		fatal("query mid", err)
	}
	midUsed := 0
	for i := 0; i < 10 && rows.Next(); i++ {
		var id, n int
		_ = rows.Scan(&id, &n)
		if i == 9 {
			st, _ := sqlitex.ReadDBStatus(ctx, conn, sqlitex.DBStatusCacheUsed, false)
			midUsed = st.Current
		}
	}
	rows.Close()

	// Now do a full scan.
	var cnt, tot int64
	if err := conn.QueryRowContext(ctx, scanSQL).Scan(&cnt, &tot); err != nil {
		fatal("full scan", err)
	}
	h := handle{db: db, conn: conn}
	used, hits, misses, _ := h.statusAll(ctx)
	rss1 := readRSS()
	enc, _ := json.Marshal(scanOut{
		CacheSize: cacheSize,
		CacheUsed: used,
		Pages:     cachedPages(used),
		MidPages:  cachedPages(midUsed),
		Hits:      hits,
		Misses:    misses,
		RSSStart:  rss0,
		RSSEnd:    rss1,
	})
	fmt.Println(string(enc))
}

type shrinkOut struct {
	BeforeSumPages int
	AfterSumPages  int
	BeforeC1Pages  int
	BeforeC16Pages int
	AfterC16Pages  int
	RSSStart       int64
	RSSBefore      int64
	RSSAfter       int64
}

func doChildShrink(dir string, count int) {
	ctx := context.Background()
	rss0 := readRSS()
	handles := make([]handle, count)
	for i := range count {
		handles[i] = openHandle(ctx, filepath.Join(dir, fmt.Sprintf("f-%02d.db", i)), -2000)
		handles[i].scan(ctx)
	}
	sumBefore := 0
	for _, h := range handles {
		sumBefore += cachedPages(h.cacheUsed(ctx))
	}
	c1Before := cachedPages(handles[0].cacheUsed(ctx))
	cLastBefore := cachedPages(handles[count-1].cacheUsed(ctx))
	rssBefore := readRSS()

	// Invoke PRAGMA shrink_memory ONLY on connection 1 (which already has 0 pages!).
	if _, err := handles[0].conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
		fatal("shrink_memory", err)
	}
	sumAfter := 0
	for _, h := range handles {
		sumAfter += cachedPages(h.cacheUsed(ctx))
	}
	cLastAfter := cachedPages(handles[count-1].cacheUsed(ctx))
	rssAfter := readRSS()

	for _, h := range handles {
		h.close()
	}
	enc, _ := json.Marshal(shrinkOut{
		BeforeSumPages: sumBefore,
		AfterSumPages:  sumAfter,
		BeforeC1Pages:  c1Before,
		BeforeC16Pages: cLastBefore,
		AfterC16Pages:  cLastAfter,
		RSSStart:       rss0,
		RSSBefore:      rssBefore,
		RSSAfter:       rssAfter,
	})
	fmt.Println(string(enc))
}

type shrinkPinOut struct {
	PinnedPagesAfterShrink int
	DirtyPagesAfterShrink  int
	CleanPagesAfterShrink  int
	AfterReleaseSumPages   int
}

func doChildShrinkPin(dbA, dbB, dbC string) {
	ctx := context.Background()
	hA := openHandle(ctx, dbA, -2000)
	hB := openHandle(ctx, dbB, -2000)
	hC := openHandle(ctx, dbC, -2000)
	defer hA.close()
	defer hB.close()
	defer hC.close()

	// hC scans clean pages.
	hC.scan(ctx)
	// hA opens a cursor and pins pages mid-scan.
	rows, err := hA.conn.QueryContext(ctx, "SELECT id, body FROM t")
	if err != nil {
		fatal("hA query", err)
	}
	if !rows.Next() {
		fatal("hA next", errors.New("no rows"))
	}
	// hB begins a write tx and dirties 40 rows without committing.
	tx, err := hB.conn.BeginTx(ctx, nil)
	if err != nil {
		fatal("hB begin", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE t SET body = zeroblob(3500) WHERE id <= 40"); err != nil {
		fatal("hB update", err)
	}

	// Run PRAGMA shrink_memory on hC.
	if _, err := hC.conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
		fatal("shrink", err)
	}
	pinAfter := cachedPages(hA.cacheUsed(ctx))
	dirtyAfter := cachedPages(hB.cacheUsed(ctx))
	cleanAfter := cachedPages(hC.cacheUsed(ctx))

	// Release cursor and rollback tx, then shrink again.
	rows.Close()
	_ = tx.Rollback()
	if _, err := hC.conn.ExecContext(ctx, "PRAGMA shrink_memory"); err != nil {
		fatal("shrink2", err)
	}
	afterRelease := cachedPages(hA.cacheUsed(ctx)) + cachedPages(hB.cacheUsed(ctx)) + cachedPages(hC.cacheUsed(ctx))

	enc, _ := json.Marshal(shrinkPinOut{
		PinnedPagesAfterShrink: pinAfter,
		DirtyPagesAfterShrink:  dirtyAfter,
		CleanPagesAfterShrink:  cleanAfter,
		AfterReleaseSumPages:   afterRelease,
	})
	fmt.Println(string(enc))
}

type softHeapOut struct {
	Conn2SawLimit  int64
	SumPages1      int
	SumPages16     int
	SumPages32     int
	SumCache32     int
	RSSGrowth1     int64
	RSSGrowth32    int64
	TinyWriteOK    bool
	TinyWriteSpill int
}

func doChildSoftHeap(dir string, count int, limit int64) {
	ctx := context.Background()
	rss0 := readRSS()
	handles := make([]handle, 0, count)
	var sum1, sum16, sum32, cache32 int
	var rss1, rss32 int64
	var conn2Limit int64

	for i := range count {
		h := openHandle(ctx, filepath.Join(dir, fmt.Sprintf("f-%02d.db", i)), -2000)
		if i == 0 {
			var got int64
			if err := h.conn.QueryRowContext(ctx, fmt.Sprintf("PRAGMA soft_heap_limit=%d", limit)).Scan(&got); err != nil {
				fatal("set soft_heap_limit", err)
			}
		} else if i == 1 {
			if err := h.conn.QueryRowContext(ctx, "PRAGMA soft_heap_limit").Scan(&conn2Limit); err != nil {
				fatal("get soft_heap_limit", err)
			}
		}
		h.scan(ctx)
		handles = append(handles, h)
		if i+1 == 1 || i+1 == 16 || i+1 == count {
			sp, sc := 0, 0
			for _, other := range handles {
				u := other.cacheUsed(ctx)
				sc += u
				sp += cachedPages(u)
			}
			r := readRSS() - rss0
			switch i + 1 {
			case 1:
				sum1, rss1 = sp, r
			case 16:
				sum16 = sp
			case count:
				sum32, cache32, rss32 = sp, sc, r
			}
		}
	}

	// Test soft (not hard) behavior: set tiny 256 KiB soft_heap_limit and run a 200-row UPDATE.
	if _, err := handles[0].conn.ExecContext(ctx, "PRAGMA soft_heap_limit=262144"); err != nil {
		fatal("tiny soft_heap_limit", err)
	}
	tx, err := handles[0].conn.BeginTx(ctx, nil)
	tinyOK := false
	tinySpill := 0
	if err == nil {
		if _, err := tx.ExecContext(ctx, "UPDATE t SET body = zeroblob(3500) WHERE id <= 200"); err == nil {
			tinyOK = true
		}
		_, _, _, tinySpill = handles[0].statusAll(ctx)
		_ = tx.Rollback()
	}
	_, _ = handles[0].conn.ExecContext(ctx, "PRAGMA soft_heap_limit=0")

	for _, h := range handles {
		h.close()
	}
	enc, _ := json.Marshal(softHeapOut{
		Conn2SawLimit:  conn2Limit,
		SumPages1:      sum1,
		SumPages16:     sum16,
		SumPages32:     sum32,
		SumCache32:     cache32,
		RSSGrowth1:     rss1,
		RSSGrowth32:    rss32,
		TinyWriteOK:    tinyOK,
		TinyWriteSpill: tinySpill,
	})
	fmt.Println(string(enc))
}

type mmapWALOut struct {
	UnmappedPages      int
	MappedWithWALPages int
	MappedAfterCkpt    int
	MappedPartialPages int
}

func doChildMmapWAL(dbPath string) {
	ctx := context.Background()
	info, err := os.Stat(dbPath)
	if err != nil {
		fatal("stat", err)
	}
	mmapFull := 4 * info.Size()

	// Disable autocheckpoint and update all rows so every leaf page lives in -wal!
	o := walOpts(-2000)
	o.Conn.WALAutocheckpoint = 0
	wdb, err := sqlitex.Open(ctx, dbPath, o)
	if err != nil {
		fatal("open writer", err)
	}
	if _, err := wdb.ExecContext(ctx, "UPDATE t SET body = zeroblob(3500)"); err != nil {
		fatal("update all", err)
	}
	// Keep wdb open so SQLite does NOT checkpoint on close while readers scan!
	plain := openHandle(ctx, dbPath, -2000)
	plain.scan(ctx)
	unmappedPages := cachedPages(plain.cacheUsed(ctx))
	plain.close()

	mapped := openHandle(ctx, dbPath, -2000)
	if _, err := mapped.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", mmapFull)); err != nil {
		fatal("mmap_size", err)
	}
	mapped.scan(ctx)
	withWALPages := cachedPages(mapped.cacheUsed(ctx))
	mapped.close()

	// Now checkpoint(TRUNCATE) and close writer.
	if _, err := wdb.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		fatal("ckpt", err)
	}
	wdb.Close()

	// Fresh mapped connection after checkpoint -> should drop to ~2 pages.
	mappedAfter := openHandle(ctx, dbPath, -2000)
	if _, err := mappedAfter.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", mmapFull)); err != nil {
		fatal("mmap_size2", err)
	}
	mappedAfter.scan(ctx)
	afterCkptPages := cachedPages(mappedAfter.cacheUsed(ctx))
	mappedAfter.close()

	// Partial mmap (half the file) after checkpoint.
	partial := openHandle(ctx, dbPath, -2000)
	if _, err := partial.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", info.Size()/2)); err != nil {
		fatal("mmap partial", err)
	}
	partial.scan(ctx)
	partialPages := cachedPages(partial.cacheUsed(ctx))
	partial.close()

	enc, _ := json.Marshal(mmapWALOut{
		UnmappedPages:      unmappedPages,
		MappedWithWALPages: withWALPages,
		MappedAfterCkpt:    afterCkptPages,
		MappedPartialPages: partialPages,
	})
	fmt.Println(string(enc))
}

type pathLenOut struct {
	ShortLen   int
	LongLen    int
	ShortUsed  int
	LongUsed   int
	ShortPages int
	LongPages  int
}

func doChildPathLen(shortPath, longPath string) {
	ctx := context.Background()
	h1 := openHandle(ctx, shortPath, 100)
	h1.scan(ctx)
	u1 := h1.cacheUsed(ctx)
	h1.close()

	h2 := openHandle(ctx, longPath, 100)
	h2.scan(ctx)
	u2 := h2.cacheUsed(ctx)
	h2.close()

	enc, _ := json.Marshal(pathLenOut{
		ShortLen:   len(shortPath),
		LongLen:    len(longPath),
		ShortUsed:  u1,
		LongUsed:   u2,
		ShortPages: cachedPages(u1),
		LongPages:  cachedPages(u2),
	})
	fmt.Println(string(enc))
}

// --- Attacks ---

// CAC-01a tests OUTLINE 4.2 bullet 1: tiny (0, 1, 2, 10, -1, -10) and huge (1000000, -1000000) cache_size.
func attackCAC01a(dbPath string) {
	sizes := []int{0, 1, 2, 10, -1, -10, 1000000, -1000000}
	var parts []string
	var cs1, cs2, csHuge, csNegHuge scanOut
	for _, sz := range sizes {
		var out scanOut
		runSelfJSON(&out, childScanRole, dbPath, strconv.Itoa(sz))
		parts = append(parts, fmt.Sprintf("%d->%dp(mid=%dp)", sz, out.Pages, out.MidPages))
		switch sz {
		case 1:
			cs1 = out
		case 2:
			cs2 = out
		case 1000000:
			csHuge = out
		case -1000000:
			csNegHuge = out
		}
	}
	// Check build-notes #8 ("A cache of N pages holds N-1") at N=1,2 AND lazy allocation for huge.
	nMinusOneHolds := cs1.Pages == 0 && cs2.Pages == 1 && cs1.MidPages <= 1
	lazyHugeHolds := csHuge.Pages == 1004 && csNegHuge.Pages == 1004 && (csHuge.RSSEnd-csHuge.RSSStart) < 100<<20
	held := nMinusOneHolds && lazyHugeHolds
	report(
		"CAC-01a",
		"tiny (0, 1, 2, 10, -1, -10) and huge (1000000, -1000000) PRAGMA cache_size on 1004-page WAL DB",
		"N-page cache holds N-1 pages (build-notes #8) including N=1,2 and never exceeds N during scan; huge cache_size allocates lazily (1004 pages)",
		fmt.Sprintf("lazyHuge=%v; %s", lazyHugeHolds, strings.Join(parts, " ")),
		held,
	)
}

func runExp42(ctx context.Context, root, workBase string, extraArgs ...string) (int, string) {
	args := append([]string{"run", "./ch04-pager-and-page-cache/experiments/4.2-cache-budget"}, extraArgs...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), sqlitex.WorkDirEnv+"="+workBase)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, string(out)
}

func extractFailedChecks(out string) string {
	var fails []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "check FAILED:") || strings.HasPrefix(line, "CACHE BUDGET FAILED:") {
			fails = append(fails, strings.TrimSpace(line))
		}
	}
	if len(fails) == 0 {
		return "none"
	}
	return strings.Join(fails, " | ")
}

// CAC-01b runs Experiment 4.2 itself with a valid positive -cache-size 500.
// 4.2/main.go:125 computes budgetBytes(500)=500*4096 and checks CACHE_USED <= budget*1.05,
// ignoring the 256B/page (+6.25%) header overhead!
func attackCAC01b(ctx context.Context, root, base string) {
	wb := filepath.Join(base, "exp42-pos")
	_ = os.MkdirAll(wb, 0o755)
	code, out := runExp42(ctx, root, wb, "-n", "4", "-k", "2", "-cache-size", "500", "-rows", "600")
	held := code == 0
	report(
		"CAC-01b",
		"run Experiment 4.2 with valid positive -cache-size 500 (-n 4 -k 2 -rows 600)",
		"Experiment 4.2 passes its own budget checks (exit 0) for positive page-count cache_size",
		fmt.Sprintf("exit=%d failed=%s", code, extractFailedChecks(out)),
		held,
	)
}

// CAC-01c runs Experiment 4.2 itself with small negative -cache-size -50 (-n 16 -k 8 -rows 200),
// exposing the pool-eviction nMax vs (nMax-1) jump that breaks Part D's +1% allowance.
func attackCAC01c(ctx context.Context, root, base string) {
	wb := filepath.Join(base, "exp42-smallneg")
	_ = os.MkdirAll(wb, 0o755)
	code, out := runExp42(ctx, root, wb, "-n", "16", "-k", "8", "-cache-size", "-50", "-rows", "200")
	held := code == 0
	report(
		"CAC-01c",
		"run Experiment 4.2 with small negative -cache-size -50 (-n 16 -k 8 -rows 200)",
		"Experiment 4.2 Part D bounded-pool check (<= K*full*1.01) holds for small cache_size",
		fmt.Sprintf("exit=%d failed=%s", code, extractFailedChecks(out)),
		held,
	)
}

// CAC-02a tests OUTLINE 4.2 bullet 2: PRAGMA shrink_memory on ONE idle connection
// wipes the cache of ALL 15 OTHER connections under ENABLE_MEMORY_MANAGEMENT,
// and measures whether RSS returns to the OS.
func attackCAC02a(filesDir string) {
	var out shrinkOut
	runSelfJSON(&out, childShrink, filesDir, "16")
	// If PRAGMA shrink_memory were connection-scoped (as SQLite's PRAGMA docs state),
	// running it on connection 1 (which already holds 0 pages) would leave connection 16's 1004 pages intact!
	perConnScoped := out.AfterC16Pages == out.BeforeC16Pages && out.AfterSumPages == out.BeforeSumPages
	rssBeforeMiB := float64(out.RSSBefore-out.RSSStart) / (1 << 20)
	rssAfterMiB := float64(out.RSSAfter-out.RSSStart) / (1 << 20)
	report(
		"CAC-02a",
		"PRAGMA shrink_memory invoked ONLY on idle connection 1 (0 cached pages) while 15 other connections hold 7712 pages",
		"per SQLite PRAGMA docs (connection-scoped): other connections' caches stay intact (conn16=1004, sum=7712)",
		fmt.Sprintf("sumPages %d->%d conn16 %d->%d (conn1 was %d); RSS growth %.1f MiB->%.1f MiB",
			out.BeforeSumPages, out.AfterSumPages, out.BeforeC16Pages, out.AfterC16Pages, out.BeforeC1Pages, rssBeforeMiB, rssAfterMiB),
		perConnScoped,
	)
}

// CAC-02b tests that PRAGMA shrink_memory cannot evict pinned cursor pages or
// uncommitted dirty pages, and frees them once unpinned/rolled back.
func attackCAC02b(filesDir string) {
	var out shrinkPinOut
	runSelfJSON(&out, childShrinkPin,
		filepath.Join(filesDir, "f-00.db"),
		filepath.Join(filesDir, "f-01.db"),
		filepath.Join(filesDir, "f-02.db"),
	)
	held := out.PinnedPagesAfterShrink > 0 && out.DirtyPagesAfterShrink >= 40 && out.CleanPagesAfterShrink == 0 && out.AfterReleaseSumPages == 0
	report(
		"CAC-02b",
		"PRAGMA shrink_memory while conn A holds an open cursor and conn B holds 40 dirty uncommitted pages",
		"clean conn C drops to 0; pinned conn A (>0) and dirty conn B (>=40) survive shrink until released, then drop to 0",
		fmt.Sprintf("afterShrink: pinnedA=%d dirtyB=%d cleanC=%d; afterRelease+shrink: sum=%d",
			out.PinnedPagesAfterShrink, out.DirtyPagesAfterShrink, out.CleanPagesAfterShrink, out.AfterReleaseSumPages),
		held,
	)
}

// CAC-03 tests OUTLINE 4.2 bullet 3: PRAGMA soft_heap_limit is process-wide and caps
// total page-cache memory across 32 open connections (breaking the unqualified claim
// that total page-cache memory grows linearly with open connections unless closed).
func attackCAC03(filesDir string) {
	const limit = 4 << 20 // 4 MiB
	var out softHeapOut
	runSelfJSON(&out, childSoftHeap, filesDir, "32", strconv.FormatInt(limit, 10))
	// Without soft_heap_limit, 32 connections at -2000 hold 32*482 = 15,424 pages (~67 MiB CACHE_USED, ~130 MiB RSS).
	// If the unqualified linear-growth claim held, SumPages32 would be ~15,424.
	grewLinearly := out.SumPages32 >= 32*482*9/10
	report(
		"CAC-03",
		"32 open connections (cache_size=-2000 each) with PRAGMA soft_heap_limit=4MiB set on connection 1",
		"per unqualified 4.2 claim (memory grows linearly with open connections unless closed): ~15424 pages (~64 MiB)",
		fmt.Sprintf("conn2SawLimit=%d pages(1->16->32)=%d->%d->%d sumCache32=%.1f MiB RSS(1->32)=%.1f->%.1f MiB tiny256KiBWriteOK=%v(spills=%d)",
			out.Conn2SawLimit, out.SumPages1, out.SumPages16, out.SumPages32, float64(out.SumCache32)/(1<<20),
			float64(out.RSSGrowth1)/(1<<20), float64(out.RSSGrowth32)/(1<<20), out.TinyWriteOK, out.TinyWriteSpill),
		grewLinearly,
	)
}

// CAC-04 attacks 4.2 Part E ("with mmap covering the file, the scan leaves CACHE_USED near zero")
// when pages reside in an uncheckpointed -wal file.
func attackCAC04(base string) {
	dbPath := filepath.Join(base, "mmap-wal.db")
	buildWALDB(context.Background(), dbPath, 500)
	var out mmapWALOut
	runSelfJSON(&out, childMmapWAL, dbPath)
	// Part E expects mapped connection CACHE_USED < 5% of unmapped (< ~24 pages).
	staysNearZeroWithWAL := out.MappedWithWALPages*20 < out.UnmappedPages
	report(
		"CAC-04",
		"full-table scan with PRAGMA mmap_size=4x file size while all 500 leaf pages sit in uncheckpointed -wal",
		"per unqualified 4.2 Part E: mmap covering the file keeps CACHE_USED near zero (<5% of unmapped)",
		fmt.Sprintf("unmapped=%dp mappedWithUncheckpointedWAL=%dp mappedAfterCheckpoint=%dp partialHalfMmap=%dp",
			out.UnmappedPages, out.MappedWithWALPages, out.MappedAfterCkpt, out.MappedPartialPages),
		staysNearZeroWithWAL,
	)
}

// CAC-05 tests build-notes #7: CACHE_USED depends on file path length (3x pathLen in Pager),
// causing 4.2's cachedPages=(cacheUsed-4096)/4352 formula to overcount on a valid ~1500B Linux path.
func attackCAC05(base, srcDB string) {
	shortPath := filepath.Join(base, "s.db")
	data, err := os.ReadFile(srcDB)
	if err != nil {
		fatal("read src", err)
	}
	if err := os.WriteFile(shortPath, data, 0o644); err != nil {
		fatal("write short", err)
	}
	// Build a valid ~1550-byte nested directory path (each component 100 chars, well under NAME_MAX=255 and PATH_MAX=4096).
	longDir := filepath.Join(base, "p")
	seg := strings.Repeat("d", 100)
	for len(longDir) < 1550 {
		longDir = filepath.Join(longDir, seg)
	}
	if err := os.MkdirAll(longDir, 0o755); err != nil {
		fatal("mkdir long", err)
	}
	longPath := filepath.Join(longDir, "l.db")
	if err := os.WriteFile(longPath, data, 0o644); err != nil {
		fatal("write long", err)
	}

	var out pathLenOut
	runSelfJSON(&out, childPathLen, shortPath, longPath)
	// With cache_size=100, both connections actually hold 99 cached pages.
	// Check whether 4.2's cachedPages formula agrees on both paths!
	formulaPathIndependent := out.ShortPages == 99 && out.LongPages == 99
	report(
		"CAC-05",
		fmt.Sprintf("open identical DB at cache_size=100 via short path (%dB) vs valid Linux long path (%dB)", out.ShortLen, out.LongLen),
		"4.2's cachedPages=(CACHE_USED-4096)/4352 reports 99 pages on both paths",
		fmt.Sprintf("short: CACHE_USED=%d -> %dp; long: CACHE_USED=%d (+%dB) -> %dp",
			out.ShortUsed, out.ShortPages, out.LongUsed, out.LongUsed-out.ShortUsed, out.LongPages),
		formulaPathIndependent,
	)
}

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case childScanRole:
			cs, _ := strconv.Atoi(os.Args[3])
			doChildScan(os.Args[2], cs)
			return
		case childShrink:
			n, _ := strconv.Atoi(os.Args[3])
			doChildShrink(os.Args[2], n)
			return
		case childShrinkPin:
			doChildShrinkPin(os.Args[2], os.Args[3], os.Args[4])
			return
		case childSoftHeap:
			n, _ := strconv.Atoi(os.Args[3])
			lim, _ := strconv.ParseInt(os.Args[4], 10, 64)
			doChildSoftHeap(os.Args[2], n, lim)
			return
		case childMmapWAL:
			doChildMmapWAL(os.Args[2])
			return
		case childPathLen:
			doChildPathLen(os.Args[2], os.Args[3])
			return
		}
	}

	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	base := workDir()
	defer os.RemoveAll(base)

	// Build one 1000-row (1004-page) WAL source DB and copy 32 times for shrink/soft_heap_limit tests.
	filesDir := filepath.Join(base, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		fatal("mkdir files", err)
	}
	src := filepath.Join(filesDir, "f-00.db")
	buildWALDB(ctx, src, 1000)
	data, err := os.ReadFile(src)
	if err != nil {
		fatal("read src", err)
	}
	for i := 1; i < 32; i++ {
		if err := os.WriteFile(filepath.Join(filesDir, fmt.Sprintf("f-%02d.db", i)), data, 0o644); err != nil {
			fatal("copy file", err)
		}
	}

	attackCAC01a(src)
	attackCAC01b(ctx, root, base)
	attackCAC01c(ctx, root, base)
	attackCAC02a(filesDir)
	attackCAC02b(filesDir)
	attackCAC03(filesDir)
	attackCAC04(base)
	attackCAC05(base, src)
}
