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

	childCalibrate   = "child-calibrate"
	childUnbounded   = "child-unbounded"
	childBounded     = "child-bounded"
	childHetero      = "child-hetero"
	childMmapScale   = "child-mmap-scale"
	childSecondPass  = "child-second-pass"
	scaleConns       = 1000
	scalePoolK       = 16
	scaleCacheKiB    = -256
	scaleRows        = 100
	calibRows        = 250
	heteroLargeKiB   = -384
	heteroSmallKiB   = -128
	secondPassConns  = 16
	secondPassRows   = 1000
	secondPassBudget = -2000
)

type procMem struct {
	RSS   int64 `json:"rss"`
	Anon  int64 `json:"anon"`
	File  int64 `json:"file"`
	Shmem int64 `json:"shmem"`
	FDs   int   `json:"fds"`
}

type scalePoint struct {
	Label        string  `json:"label"`
	Open         int     `json:"open"`
	Visited      int     `json:"visited"`
	SumPages     int     `json:"sum_pages"`
	MinPages     int     `json:"min_pages"`
	MaxPages     int     `json:"max_pages"`
	SumCacheUsed int64   `json:"sum_cache_used"`
	Mem          procMem `json:"mem"`
}

type calibOut struct {
	PerPage   int `json:"per_page"`
	FullScale int `json:"full_scale"`
	FullLarge int `json:"full_large"`
	FullSmall int `json:"full_small"`
	Full2000  int `json:"full_2000"`
}

type heteroOut struct {
	Start        procMem `json:"start"`
	At1000Pages  int     `json:"at1000_pages"`
	At1000Mem    procMem `json:"at1000_mem"`
	After500Page int     `json:"after500_pages"`
	After500Mem  procMem `json:"after500_mem"`
	ClosedPages  int     `json:"closed_pages"`
	ClosedMem    procMem `json:"closed_mem"`
}

type mmapScaleOut struct {
	Start    procMem `json:"start"`
	At1000   procMem `json:"at1000"`
	SumPages int     `json:"sum_pages"`
	MinPages int     `json:"min_pages"`
	MaxPages int     `json:"max_pages"`
}

type secondPassOut struct {
	Pass1Sum    int `json:"pass1_sum"`
	Pass1Min    int `json:"pass1_min"`
	Pass1Max    int `json:"pass1_max"`
	Pass2Sum    int `json:"pass2_sum"`
	Pass2Min    int `json:"pass2_min"`
	Pass2Max    int `json:"pass2_max"`
	Pass2MaxSum int `json:"pass2_max_sum"`
}

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
	dir := filepath.Join(root, "_work", "adv-ch04", "cache-scale")
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	return dir
}

// readProcMem samples RSS components from /proc/self/status and open FD count
// from /proc/self/fd on Linux. On non-Linux builds (e.g. darwin cross-build),
// it returns -1 sentinels so the package builds cleanly everywhere.
func readProcMem() procMem {
	runtime.GC()
	debug.FreeOSMemory()
	m := procMem{RSS: -1, Anon: -1, File: -1, Shmem: -1, FDs: -1}
	if runtime.GOOS != "linux" {
		return m
	}
	if f, err := os.Open("/proc/self/status"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				continue
			}
			switch fields[0] {
			case "VmRSS:":
				m.RSS = kib * 1024
			case "RssAnon:":
				m.Anon = kib * 1024
			case "RssFile:":
				m.File = kib * 1024
			case "RssShmem:":
				m.Shmem = kib * 1024
			}
		}
		f.Close()
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		m.FDs = len(entries)
	}
	return m
}

func openOpts(cacheSize int) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.SingleConnection = true
	o.Conn.CacheSize = cacheSize
	return o
}

func buildWALDB(ctx context.Context, path string, rows int) int64 {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove db", err)
	}
	db, err := sqlitex.Open(ctx, path, openOpts(-2000))
	if err != nil {
		fatal("open build", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create table", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	rng := rand.New(rand.NewPCG(4, 200))
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
		fatal("close build", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		fatal("stat build", err)
	}
	return info.Size()
}

type handle struct {
	db   *sqlitex.DB
	conn *sql.Conn
}

func openHandle(ctx context.Context, path string, cacheSize int) handle {
	db, err := sqlitex.Open(ctx, path, openOpts(cacheSize))
	if err != nil {
		fatal("open "+path, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn "+path, err)
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
	var c, s int64
	if err := h.conn.QueryRowContext(ctx, scanSQL).Scan(&c, &s); err != nil {
		fatal("scan", err)
	}
}

func (h handle) cacheUsed(ctx context.Context) int {
	st, err := sqlitex.ReadDBStatus(ctx, h.conn, sqlitex.DBStatusCacheUsed, false)
	if err != nil {
		fatal("db_status", err)
	}
	return st.Current
}

func cachedPages(cacheUsed, perPage int) int {
	return max(0, (cacheUsed-pageSize)/perPage)
}

func sampleHandles(ctx context.Context, hs []handle, perPage int, label string, visited int) scalePoint {
	p := scalePoint{Label: label, Open: len(hs), Visited: visited}
	if len(hs) > 0 {
		p.MinPages = 1 << 30
	}
	for _, h := range hs {
		cu := h.cacheUsed(ctx)
		pg := cachedPages(cu, perPage)
		p.SumCacheUsed += int64(cu)
		p.SumPages += pg
		if pg < p.MinPages {
			p.MinPages = pg
		}
		if pg > p.MaxPages {
			p.MaxPages = pg
		}
	}
	p.Mem = readProcMem()
	return p
}

func filePath(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf("f-%04d.db", i))
}

func runSelfJSON(dst any, args ...string) {
	exe, err := os.Executable()
	if err != nil {
		fatal("executable", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal("child "+args[0], err)
	}
	if err := json.Unmarshal(out, dst); err != nil {
		fatal("unmarshal "+args[0], fmt.Errorf("%w: %s", err, string(out)))
	}
}

func emitJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fatal("marshal", err)
	}
	fmt.Println(string(b))
}

func doChildCalibrate(calibDB, scaleDB, bigDB string) {
	ctx := context.Background()
	measureOne := func(path string, cs int) int {
		h := openHandle(ctx, path, cs)
		defer h.close()
		h.scan(ctx)
		return h.cacheUsed(ctx)
	}
	c100 := measureOne(calibDB, 100)
	c200 := measureOne(calibDB, 200)
	if c200 <= c100 || (c200-c100)%100 != 0 {
		fatal("calibrate", fmt.Errorf("c200=%d c100=%d", c200, c100))
	}
	perPage := (c200 - c100) / 100
	emitJSON(calibOut{
		PerPage:   perPage,
		FullScale: cachedPages(measureOne(scaleDB, scaleCacheKiB), perPage),
		FullLarge: cachedPages(measureOne(scaleDB, heteroLargeKiB), perPage),
		FullSmall: cachedPages(measureOne(scaleDB, heteroSmallKiB), perPage),
		Full2000:  cachedPages(measureOne(bigDB, secondPassBudget), perPage),
	})
}

func doChildUnbounded(dir string, n, cacheSize, perPage int, oneFile bool) {
	ctx := context.Background()
	marks := []int{1, 100, 250, 500, n}
	var pts []scalePoint
	pts = append(pts, scalePoint{Label: "start", Mem: readProcMem()})
	handles := make([]handle, 0, n)
	for i := range n {
		path := filePath(dir, i)
		if oneFile {
			path = filePath(dir, 0)
		}
		h := openHandle(ctx, path, cacheSize)
		h.scan(ctx)
		handles = append(handles, h)
		if slices.Contains(marks, i+1) {
			pts = append(pts, sampleHandles(ctx, handles, perPage, fmt.Sprintf("after %d", i+1), i+1))
		}
	}
	for _, h := range handles {
		h.close()
	}
	pts = append(pts, scalePoint{Label: "all closed", Visited: n, Mem: readProcMem()})
	emitJSON(pts)
}

func doChildBounded(dir string, n, k, cacheSize, perPage int) {
	ctx := context.Background()
	marks := []int{k, 100, 250, 500, n}
	var pts []scalePoint
	pts = append(pts, scalePoint{Label: "start", Mem: readProcMem()})
	pool := make([]handle, 0, k)
	for i := range n {
		if len(pool) == k {
			pool[0].close()
			pool = pool[1:]
		}
		h := openHandle(ctx, filePath(dir, i), cacheSize)
		h.scan(ctx)
		pool = append(pool, h)
		if slices.Contains(marks, i+1) {
			pts = append(pts, sampleHandles(ctx, pool, perPage, fmt.Sprintf("after %d", i+1), i+1))
		}
	}
	for _, h := range pool {
		h.close()
	}
	pts = append(pts, scalePoint{Label: "all closed", Visited: n, Mem: readProcMem()})
	emitJSON(pts)
}

func doChildHetero(dbPath string, perPage int) {
	ctx := context.Background()
	var out heteroOut
	out.Start = readProcMem()

	small := make([]handle, 0, 500)
	large := make([]handle, 0, 500)
	// Open 500 small-cache connections first, then 500 large-cache connections
	// so the large-cache connections hold the active pages in the shared PGroup.
	for range 500 {
		h := openHandle(ctx, dbPath, heteroSmallKiB)
		h.scan(ctx)
		small = append(small, h)
	}
	for range 500 {
		h := openHandle(ctx, dbPath, heteroLargeKiB)
		h.scan(ctx)
		large = append(large, h)
	}
	for _, h := range small {
		out.At1000Pages += cachedPages(h.cacheUsed(ctx), perPage)
	}
	for _, h := range large {
		out.At1000Pages += cachedPages(h.cacheUsed(ctx), perPage)
	}
	out.At1000Mem = readProcMem()

	// Closing the 500 large-cache connections shrinks PGroup.nMaxPage and must
	// immediately enforce the reduced group limit via pcache1EnforceMaxPage.
	for _, h := range large {
		h.close()
	}
	for _, h := range small {
		out.After500Page += cachedPages(h.cacheUsed(ctx), perPage)
	}
	out.After500Mem = readProcMem()

	for _, h := range small {
		h.close()
	}
	out.ClosedPages = 0
	out.ClosedMem = readProcMem()
	emitJSON(out)
}

func doChildMmapScale(dir string, n, cacheSize, perPage int, mmapSize int64, oneFile bool) {
	ctx := context.Background()
	var out mmapScaleOut
	out.Start = readProcMem()
	handles := make([]handle, 0, n)
	for i := range n {
		path := filePath(dir, i)
		if oneFile {
			path = filePath(dir, 0)
		}
		h := openHandle(ctx, path, cacheSize)
		var got int64
		if err := h.conn.QueryRowContext(ctx, fmt.Sprintf("PRAGMA mmap_size=%d", mmapSize)).Scan(&got); err != nil {
			fatal("mmap_size", err)
		}
		if got != mmapSize {
			fatal("mmap_size", fmt.Errorf("got %d want %d", got, mmapSize))
		}
		h.scan(ctx)
		handles = append(handles, h)
	}
	pt := sampleHandles(ctx, handles, perPage, "after 1000", n)
	out.At1000 = pt.Mem
	out.SumPages = pt.SumPages
	out.MinPages = pt.MinPages
	out.MaxPages = pt.MaxPages
	for _, h := range handles {
		h.close()
	}
	emitJSON(out)
}

func doChildSecondPass(dir string, n, cacheSize, perPage int) {
	ctx := context.Background()
	handles := make([]handle, 0, n)
	for i := range n {
		h := openHandle(ctx, filePath(dir, i), cacheSize)
		h.scan(ctx)
		handles = append(handles, h)
	}
	p1 := sampleHandles(ctx, handles, perPage, "pass1", n)
	maxSum := p1.SumPages
	for i := range n {
		handles[i].scan(ctx)
		cur := sampleHandles(ctx, handles, perPage, "mid", n)
		if cur.SumPages > maxSum {
			maxSum = cur.SumPages
		}
	}
	p2 := sampleHandles(ctx, handles, perPage, "pass2", n)
	for _, h := range handles {
		h.close()
	}
	emitJSON(secondPassOut{
		Pass1Sum:    p1.SumPages,
		Pass1Min:    p1.MinPages,
		Pass1Max:    p1.MaxPages,
		Pass2Sum:    p2.SumPages,
		Pass2Min:    p2.MinPages,
		Pass2Max:    p2.MaxPages,
		Pass2MaxSum: maxSum,
	})
}

func findPt(pts []scalePoint, label string) scalePoint {
	for _, p := range pts {
		if p.Label == label {
			return p
		}
	}
	fatal("missing checkpoint", errors.New(label))
	return scalePoint{}
}

func mib(b int64) string {
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}

// SCL-01 checks OUTLINE 4.2 bullet 4 ("check the bound holds for 1,000 connections")
// across 1,000 distinct WAL files, scaled to cache_size=-256 KiB on 100-row DBs so
// peak RSS (~600 MiB) fits comfortably inside the 4-core / 7 GiB VM.
func attackSCL01(filesDir string, filePages int, cal calibOut) ([]scalePoint, int64) {
	var pts []scalePoint
	runSelfJSON(&pts, childUnbounded, filesDir, strconv.Itoa(scaleConns), strconv.Itoa(scaleCacheKiB), strconv.Itoa(cal.PerPage), "false")

	start := findPt(pts, "start")
	p500 := findPt(pts, "after 500")
	p1000 := findPt(pts, "after 1000")
	closed := findPt(pts, "all closed")

	sumsExact := true
	for _, p := range pts {
		if p.Open > 0 && p.SumPages != p.Open*cal.FullScale {
			sumsExact = false
		}
	}
	pooledLRU := p1000.MaxPages > cal.FullScale && p1000.MinPages < cal.FullScale

	rss500 := p500.Mem.RSS - start.Mem.RSS
	rss1000 := p1000.Mem.RSS - start.Mem.RSS
	rssClosed := closed.Mem.RSS - start.Mem.RSS
	per500 := float64(rss500) / 500.0
	per1000 := float64(rss1000) / 1000.0

	rssOK := true
	if start.Mem.RSS >= 0 {
		rssOK = rss1000 >= p1000.SumCacheUsed*9/10 && per1000 >= per500*0.80 && per1000 <= per500*1.25
	}
	held := sumsExact && pooledLRU && rssOK
	report(
		"SCL-01",
		fmt.Sprintf("1,000 open connections to 1,000 distinct %d-page WAL files (cache_size=%d KiB, full=%dp, scaled from -2000 for 7 GiB RAM)", filePages, scaleCacheKiB, cal.FullScale),
		fmt.Sprintf("sum(pages)==open*%d at every checkpoint (1000->%d), unified PGroup redistribution, RSS>=0.9*CACHE_USED, linear RSS per conn (500 vs 1000 within -20%%/+25%%)", cal.FullScale, 1000*cal.FullScale),
		fmt.Sprintf("sumsExact=%v pages1000=%d (min..max=%d..%d) RSS(500->1000)=%s->%s (perConn %.2f->%.2f MiB, ratio=%.2f) FDs=%d afterCloseRSS=%s",
			sumsExact, p1000.SumPages, p1000.MinPages, p1000.MaxPages, mib(rss500), mib(rss1000),
			per500/(1<<20), per1000/(1<<20), float64(rss1000)/float64(p1000.SumCacheUsed), p1000.Mem.FDs, mib(rssClosed)),
		held,
	)
	return pts, rss1000
}

// SCL-02 checks 4.2 Part C at 1,000 connections: 1,000 open connections to ONE
// WAL file do not share a page cache and match the 1,000-file totals checkpoint by checkpoint.
func attackSCL02(filesDir string, filePages int, cal calibOut, distPts []scalePoint) {
	var pts []scalePoint
	runSelfJSON(&pts, childUnbounded, filesDir, strconv.Itoa(scaleConns), strconv.Itoa(scaleCacheKiB), strconv.Itoa(cal.PerPage), "true")

	start := findPt(pts, "start")
	p500 := findPt(pts, "after 500")
	p1000 := findPt(pts, "after 1000")
	closed := findPt(pts, "all closed")

	matchesDist := len(pts) == len(distPts)
	for i := range pts {
		if i < len(distPts) && (pts[i].Label != distPts[i].Label || pts[i].SumPages != distPts[i].SumPages) {
			matchesDist = false
		}
	}
	rss500 := p500.Mem.RSS - start.Mem.RSS
	rss1000 := p1000.Mem.RSS - start.Mem.RSS
	rssClosed := closed.Mem.RSS - start.Mem.RSS
	per500 := float64(rss500) / 500.0
	per1000 := float64(rss1000) / 1000.0
	rssOK := true
	if start.Mem.RSS >= 0 {
		rssOK = rss1000 >= p1000.SumCacheUsed*9/10 && per1000 >= per500*0.80 && per1000 <= per500*1.25
	}
	held := matchesDist && p1000.SumPages == 1000*cal.FullScale && rssOK
	report(
		"SCL-02",
		fmt.Sprintf("1,000 open connections to ONE %d-page WAL file (cache_size=%d KiB, full=%dp)", filePages, scaleCacheKiB, cal.FullScale),
		fmt.Sprintf("no shared cache: pages match 1,000-file run at every checkpoint (1000->%d) and RSS grows linearly", 1000*cal.FullScale),
		fmt.Sprintf("matches1000Files=%v pages1000=%d RSS(500->1000)=%s->%s (perConn %.2f->%.2f MiB) FDs=%d afterCloseRSS=%s",
			matchesDist, p1000.SumPages, mib(rss500), mib(rss1000), per500/(1<<20), per1000/(1<<20), p1000.Mem.FDs, mib(rssClosed)),
		held,
	)
}

// SCL-03 tests 4.2 Part D's bounded LRU pool at 1,000 distinct WAL files (K=16):
// checks whether cached pages, open FDs, and process RSS truly stay flat across
// 984 LRU evictions (watching for RSS creep over long eviction runs).
func attackSCL03(filesDir string, cal calibOut, unboundedRSS1000 int64) {
	var pts []scalePoint
	runSelfJSON(&pts, childBounded, filesDir, strconv.Itoa(scaleConns), strconv.Itoa(scalePoolK), strconv.Itoa(scaleCacheKiB), strconv.Itoa(cal.PerPage))

	start := findPt(pts, "start")
	pK := findPt(pts, fmt.Sprintf("after %d", scalePoolK))
	p100 := findPt(pts, "after 100")
	p500 := findPt(pts, "after 500")
	p1000 := findPt(pts, "after 1000")

	maxAllowedPages := scalePoolK * (cal.FullScale + 1)
	pagesOK := true
	maxObsPages := 0
	for _, p := range pts {
		if p.Open > 0 {
			if p.SumPages > maxObsPages {
				maxObsPages = p.SumPages
			}
			if p.Open != scalePoolK || p.SumPages > maxAllowedPages {
				pagesOK = false
			}
		}
	}
	rssK := pK.Mem.RSS - start.Mem.RSS
	rss100 := p100.Mem.RSS - start.Mem.RSS
	rss500 := p500.Mem.RSS - start.Mem.RSS
	rss1000 := p1000.Mem.RSS - start.Mem.RSS

	flatRSS := true
	fracOK := true
	fdOK := true
	if start.Mem.RSS >= 0 {
		// Mirror Experiment 4.2 Part D's exact RSS bounds:
		// (1) growth from after K to after N <= atK / 4
		// (2) final pooled growth <= 1.5 * (K / N) * unbounded growth
		flatRSS = (rss1000 - rssK) <= rssK/4
		fracOK = float64(rss1000) <= float64(unboundedRSS1000)*(float64(scalePoolK)/float64(scaleConns))*1.5
		fdOK = p1000.Mem.FDs <= 3*scalePoolK+32
	}
	held := pagesOK && flatRSS && fracOK && fdOK
	report(
		"SCL-03",
		fmt.Sprintf("1,000 distinct WAL files visited through K=%d LRU pool (cache_size=%d KiB, 984 evictions)", scalePoolK, scaleCacheKiB),
		fmt.Sprintf("open=%d, pages<=%d (%d*(%d+1)), flat RSS after K (delta<=25%% of atK), final RSS<=1.5*(16/1000) of unbounded (%s), FDs<=%d",
			scalePoolK, maxAllowedPages, scalePoolK, cal.FullScale,
			mib(int64(float64(unboundedRSS1000)*(float64(scalePoolK)/float64(scaleConns))*1.5)), 3*scalePoolK+32),
		fmt.Sprintf("maxPages=%d/%d RSS(16->100->500->1000)=%s->%s->%s->%s flatOK=%v fracOK=%v FDs=%d",
			maxObsPages, maxAllowedPages, mib(rssK), mib(rss100), mib(rss500), mib(rss1000), flatRSS, fracOK, p1000.Mem.FDs),
		held,
	)
}

// SCL-04 tests 1,000 connections with heterogeneous cache_size (500 at -384 KiB +
// 500 at -128 KiB), verifying that closing the 500 large-cache connections
// immediately shrinks PGroup pages to <= 500 * nMaxSmall and checking RSS release
// after closing all 1,000 connections.
func attackSCL04(dbPath string, cal calibOut) {
	var out heteroOut
	runSelfJSON(&out, childHetero, dbPath, strconv.Itoa(cal.PerPage))

	nMaxLarge := cal.FullLarge + 1
	nMaxSmall := cal.FullSmall + 1
	max1000 := 500*nMaxLarge + 500*nMaxSmall
	max500 := 500 * nMaxSmall

	rss1000 := out.At1000Mem.RSS - out.Start.RSS
	rss500 := out.After500Mem.RSS - out.Start.RSS
	rssClosed := out.ClosedMem.RSS - out.Start.RSS

	pages1000OK := out.At1000Pages <= max1000 && out.At1000Pages >= 500*cal.FullLarge+500*cal.FullSmall
	shrink500OK := out.After500Page <= max500
	rssReleased := true
	if out.Start.RSS >= 0 {
		// Closing all 1,000 connections should release at least 75% of peak RSS.
		rssReleased = rssClosed <= rss1000/4
	}
	held := pages1000OK && shrink500OK && rssReleased
	report(
		"SCL-04",
		fmt.Sprintf("1,000 connections with mixed cache_size (500x%d KiB + 500x%d KiB), close 500 large, then close all 1,000", heteroLargeKiB, heteroSmallKiB),
		fmt.Sprintf("at1000 pages<=%d, after closing 500 large pages<=%d immediately, after closing all 1,000 RSS drops >=75%%", max1000, max500),
		fmt.Sprintf("pages(1000->500->0)=%d->%d->%d RSS(1000->500->closed)=%s->%s->%s (retained=%.1f%%)",
			out.At1000Pages, out.After500Page, out.ClosedPages, mib(rss1000), mib(rss500), mib(rssClosed),
			100.0*float64(rssClosed)/float64(max(int64(1), rss1000))),
		held,
	)
}

// SCL-05 tests 4.2 Part E (mmap) at 1,000 connections:
// (a) 1,000 distinct 104-page files with mmap_size = 2*fileSize
// (b) 1,000 connections to ONE 104-page file with mmap_size = 2*fileSize
// confirming CACHE_USED stays near zero in both, while RssFile scales with distinct
// mapped files (not connections to the same file).
func attackSCL05(filesDir string, fileSize int64, cal calibOut, distPts []scalePoint) {
	mmapSize := 2 * fileSize
	var distOut, oneOut mmapScaleOut
	runSelfJSON(&distOut, childMmapScale, filesDir, strconv.Itoa(scaleConns), strconv.Itoa(scaleCacheKiB), strconv.Itoa(cal.PerPage), strconv.FormatInt(mmapSize, 10), "false")
	runSelfJSON(&oneOut, childMmapScale, filesDir, strconv.Itoa(scaleConns), strconv.Itoa(scaleCacheKiB), strconv.Itoa(cal.PerPage), strconv.FormatInt(mmapSize, 10), "true")

	unmappedPages := 1000 * cal.FullScale
	unmappedStart := findPt(distPts, "start")
	unmapped1000 := findPt(distPts, "after 1000")
	unmappedAnon := unmapped1000.Mem.Anon - unmappedStart.Mem.Anon

	distFileDelta := distOut.At1000.File - distOut.Start.File
	distAnonDelta := distOut.At1000.Anon - distOut.Start.Anon
	oneFileDelta := oneOut.At1000.File - oneOut.Start.File
	oneAnonDelta := oneOut.At1000.Anon - oneOut.Start.Anon

	expectedDistFile := int64(scaleConns) * fileSize
	pagesLow := distOut.SumPages*20 < unmappedPages && oneOut.SumPages*20 < unmappedPages
	memOK := true
	if distOut.Start.File >= 0 {
		// Distinct files: file-backed RSS grows by ~1000 * fileSize and anon is far below unmapped anon.
		// Single file: kernel shares the single file's page cache, so RssFile stays << 1000 * fileSize.
		memOK = distFileDelta >= expectedDistFile*8/10 &&
			distAnonDelta < unmappedAnon/2 &&
			oneFileDelta < expectedDistFile/10
	}
	held := pagesLow && memOK
	report(
		"SCL-05",
		fmt.Sprintf("1,000 open connections with PRAGMA mmap_size=%d (2x file) on 1,000 distinct files vs 1 file", mmapSize),
		fmt.Sprintf("both keep cached pages <5%% of unmapped (%d), distinct RssFile>=80%% of %s, 1-file RssFile<10%% (shared kernel cache)",
			unmappedPages, mib(expectedDistFile)),
		fmt.Sprintf("distinct: pages=%d RssFile=+%s RssAnon=+%s; oneFile: pages=%d RssFile=+%s RssAnon=+%s (unmappedAnon=+%s)",
			distOut.SumPages, mib(distFileDelta), mib(distAnonDelta),
			oneOut.SumPages, mib(oneFileDelta), mib(oneAnonDelta), mib(unmappedAnon)),
		held,
	)
}

// SCL-06 runs Experiment 4.2 itself as a subprocess at 4x default connection count
// (-n 256 -k 16 -cache-size -1024 -rows 320, minimum allowed 1 MiB budget) with
// COURSE_WORK_DIR isolated under _work/adv-ch04/cache-scale/exp42.
func attackSCL06(ctx context.Context, root, base string) {
	expWork := filepath.Join(base, "exp42")
	t0 := time.Now()
	cmd := exec.CommandContext(ctx, "go", "run", "./ch04-pager-and-page-cache/experiments/4.2-cache-budget",
		"-n", "256", "-k", "16", "-cache-size", "-1024", "-rows", "320")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), sqlitex.WorkDirEnv+"="+expWork)
	out, err := cmd.CombinedOutput()
	dur := time.Since(t0)

	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	text := string(out)
	var failedLines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "check FAILED:") || strings.Contains(line, "FATAL") {
			failedLines = append(failedLines, strings.TrimSpace(line))
		}
	}
	held := code == 0 && strings.Contains(text, "CACHE BUDGET OK")
	failSummary := "none"
	if len(failedLines) > 0 {
		failSummary = strings.Join(failedLines, " | ")
	}
	report(
		"SCL-06",
		"run Experiment 4.2 itself at 4x scale (-n 256 -k 16 -cache-size -1024 -rows 320)",
		"Experiment 4.2 exits 0 and prints CACHE BUDGET OK at 256 connections and 1 MiB minimum budget",
		fmt.Sprintf("exit=%d ok=%v dur=%s failed=%s", code, strings.Contains(text, "CACHE BUDGET OK"), dur.Round(100*time.Millisecond), failSummary),
		held,
	)
}

// SCL-07 attacks 4.2 Part B's exact-sum claim ("the cached pages of all open
// connections add up to exactly (open connections) x 482") by performing a second
// round-robin scan across 16 open connections whose earlier entries were evicted to
// 0 pages during pass 1. Because pcache1FetchStage2 checks per-cache nPage+1>=nMax
// (pcache1.c:902) while pcache1Unpin enforces pGroup->nPurgeable>pGroup->nMaxPage
// (pcache1.c:1096), re-scanning an evicted connection grows the group sum from
// N*(nMax-1) up to N*nMax.
func attackSCL07(bigDir string, cal calibOut) {
	var out secondPassOut
	runSelfJSON(&out, childSecondPass, bigDir, strconv.Itoa(secondPassConns), strconv.Itoa(secondPassBudget), strconv.Itoa(cal.PerPage))

	partBExact := secondPassConns * cal.Full2000
	groupMax := secondPassConns * (cal.Full2000 + 1)
	staysAtPartBExact := out.Pass2Sum == partBExact && out.Pass2MaxSum == partBExact
	report(
		"SCL-07",
		fmt.Sprintf("second round-robin scan across %d open WAL connections (cache_size=%d, full=%dp, nMax=%dp)", secondPassConns, secondPassBudget, cal.Full2000, cal.Full2000+1),
		fmt.Sprintf("per 4.2 Part B / build-notes #5 ('SUM is always exactly open x %d'): stays at %d pages across pass 2", cal.Full2000, partBExact),
		fmt.Sprintf("pass1=%d (%d..%d) pass2=%d (%d..%d) pass2Max=%d (group nMaxPage=%d, delta=+%d pages)",
			out.Pass1Sum, out.Pass1Min, out.Pass1Max, out.Pass2Sum, out.Pass2Min, out.Pass2Max, out.Pass2MaxSum, groupMax, out.Pass2MaxSum-partBExact),
		staysAtPartBExact,
	)
}

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case childCalibrate:
			doChildCalibrate(os.Args[2], os.Args[3], os.Args[4])
			return
		case childUnbounded:
			n, _ := strconv.Atoi(os.Args[3])
			cs, _ := strconv.Atoi(os.Args[4])
			pp, _ := strconv.Atoi(os.Args[5])
			doChildUnbounded(os.Args[2], n, cs, pp, os.Args[6] == "true")
			return
		case childBounded:
			n, _ := strconv.Atoi(os.Args[3])
			k, _ := strconv.Atoi(os.Args[4])
			cs, _ := strconv.Atoi(os.Args[5])
			pp, _ := strconv.Atoi(os.Args[6])
			doChildBounded(os.Args[2], n, k, cs, pp)
			return
		case childHetero:
			pp, _ := strconv.Atoi(os.Args[3])
			doChildHetero(os.Args[2], pp)
			return
		case childMmapScale:
			n, _ := strconv.Atoi(os.Args[3])
			cs, _ := strconv.Atoi(os.Args[4])
			pp, _ := strconv.Atoi(os.Args[5])
			ms, _ := strconv.ParseInt(os.Args[6], 10, 64)
			doChildMmapScale(os.Args[2], n, cs, pp, ms, os.Args[7] == "true")
			return
		case childSecondPass:
			n, _ := strconv.Atoi(os.Args[3])
			cs, _ := strconv.Atoi(os.Args[4])
			pp, _ := strconv.Atoi(os.Args[5])
			doChildSecondPass(os.Args[2], n, cs, pp)
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

	// Build calibration DB (250 rows), 1,000 small scale DBs (100 rows = 104 pages),
	// and 16 full-size DBs (1000 rows = 1004 pages) for SCL-07.
	calibDB := filepath.Join(base, "calib.db")
	buildWALDB(ctx, calibDB, calibRows)

	filesDir := filepath.Join(base, "files-1000")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		fatal("mkdir files-1000", err)
	}
	srcSmall := filePath(filesDir, 0)
	smallSize := buildWALDB(ctx, srcSmall, scaleRows)
	smallBytes, err := os.ReadFile(srcSmall)
	if err != nil {
		fatal("read small src", err)
	}
	for i := 1; i < scaleConns; i++ {
		if err := os.WriteFile(filePath(filesDir, i), smallBytes, 0o644); err != nil {
			fatal("copy small", err)
		}
	}

	bigDir := filepath.Join(base, "files-big")
	if err := os.MkdirAll(bigDir, 0o755); err != nil {
		fatal("mkdir files-big", err)
	}
	srcBig := filePath(bigDir, 0)
	buildWALDB(ctx, srcBig, secondPassRows)
	bigBytes, err := os.ReadFile(srcBig)
	if err != nil {
		fatal("read big src", err)
	}
	for i := 1; i < secondPassConns; i++ {
		if err := os.WriteFile(filePath(bigDir, i), bigBytes, 0o644); err != nil {
			fatal("copy big", err)
		}
	}

	var cal calibOut
	runSelfJSON(&cal, childCalibrate, calibDB, srcSmall, srcBig)

	smallPages := int(smallSize / pageSize)
	distPts, rss1000 := attackSCL01(filesDir, smallPages, cal)
	attackSCL02(filesDir, smallPages, cal, distPts)
	attackSCL03(filesDir, cal, rss1000)
	attackSCL04(srcSmall, cal)
	attackSCL05(filesDir, smallSize, cal, distPts)
	attackSCL06(ctx, root, base)
	attackSCL07(bigDir, cal)
}
