// Group 2 adversarial attacks for Chapter 7, Experiment 7.2 (io-cost-by-pattern).
// Attacks:
//
//	COST-01: Recompute all 12 cells of 7.2 using the pinned C sqlite3 CLI under strace.
//	COST-02: synchronous=OFF across all 6 cells (delete/wal x 1x1000, 20x50, 1000x1).
//	COST-03: 20-run count stability across delete/wal and NORMAL/FULL (including full wal NORMAL 1000x1).
//	COST-04: WAL/NORMAL sync sensitivity to wal_autocheckpoint (0 vs 100 vs 1000).
//	COST-05: synchronous=EXTRA (5 syncs/commit) and PERSIST/TRUNCATE journal reuse (eliminating dir syncs).
//	COST-06: Blocked reader pinning WAL past wal_autocheckpoint under WAL/NORMAL.
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const (
	scratchRoot = "_work/adv-ch07/io-cost-falsify"
	dbName      = "t.db"
	markerName  = "commit-marker"
	valueBytes  = 100
	workers     = 4
)

type cell struct {
	journal  string
	syncMode string
	total    int
	batch    int
	autockpt int
}

func (c cell) commits() int { return c.total / c.batch }

func (c cell) label() string {
	return fmt.Sprintf("%s/%s/%dx%d(ckpt=%d)", c.journal, c.syncMode, c.commits(), c.batch, c.autockpt)
}

type expectedRow struct {
	writes    int
	bytes     int64
	syncs     int
	syncBy    string
	opens     string
	unlinks   string
	truncates string
	closeW    int
	closeSync int
}

// Exact numbers recorded in ch07-vfs-and-filesystem-io/evidence/7.2-io-cost-by-pattern.txt.
var evidence72 = map[cell]expectedRow{
	{sqlitex.JournalDelete, sqlitex.SyncNormal, 1000, 1000, 1000}: {38, 131612, 3, "db:1 dir:1 journal:1", "dir:1 journal:1", "journal:1", "-", 0, 0},
	{sqlitex.JournalDelete, sqlitex.SyncNormal, 1000, 50, 1000}:   {304, 608968, 60, "db:20 dir:20 journal:20", "dir:20 journal:20", "journal:20", "-", 0, 0},
	{sqlitex.JournalDelete, sqlitex.SyncNormal, 1000, 1, 1000}:    {10028, 17038688, 3000, "db:1000 dir:1000 journal:1000", "dir:1000 journal:1000", "journal:1000", "-", 0, 0},
	{sqlitex.JournalDelete, sqlitex.SyncFull, 1000, 1000, 1000}:   {38, 131612, 4, "db:1 dir:1 journal:2", "dir:1 journal:1", "journal:1", "-", 0, 0},
	{sqlitex.JournalDelete, sqlitex.SyncFull, 1000, 50, 1000}:     {304, 608968, 80, "db:20 dir:20 journal:40", "dir:20 journal:20", "journal:20", "-", 0, 0},
	{sqlitex.JournalDelete, sqlitex.SyncFull, 1000, 1, 1000}:      {10028, 17038688, 4000, "db:1000 dir:1000 journal:2000", "dir:1000 journal:1000", "journal:1000", "-", 0, 0},
	{sqlitex.JournalWAL, sqlitex.SyncNormal, 1000, 1000, 1000}:    {60, 123600, 0, "-", "-", "-", "-", 30, 2},
	{sqlitex.JournalWAL, sqlitex.SyncNormal, 1000, 50, 1000}:      {174, 358440, 0, "-", "-", "-", "-", 30, 2},
	{sqlitex.JournalWAL, sqlitex.SyncNormal, 1000, 1, 1000}:       {2139, 4461320, 3, "db:1 wal:2", "-", "-", "db:1", 5, 2},
	{sqlitex.JournalWAL, sqlitex.SyncFull, 1000, 1000, 1000}:      {60, 123600, 1, "wal:1", "-", "-", "-", 30, 2},
	{sqlitex.JournalWAL, sqlitex.SyncFull, 1000, 50, 1000}:        {174, 358440, 20, "wal:20", "-", "-", "-", 30, 2},
	{sqlitex.JournalWAL, sqlitex.SyncFull, 1000, 1, 1000}:         {2139, 4461320, 1003, "db:1 wal:1002", "-", "-", "db:1", 5, 2},
}

func cells12() []cell {
	var out []cell
	for _, j := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		for _, s := range []string{sqlitex.SyncNormal, sqlitex.SyncFull} {
			for _, b := range []int{1000, 50, 1} {
				out = append(out, cell{j, s, 1000, b, 1000})
			}
		}
	}
	return out
}

func emit(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", msg, err)
	os.Exit(1)
}

func cleanDir(path string) string {
	if err := os.RemoveAll(path); err != nil {
		fatal("remove "+path, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		fatal("mkdir "+path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		fatal("abs "+path, err)
	}
	return abs
}

func value(id int) []byte {
	v := make([]byte, valueBytes)
	for i := range v {
		v[i] = byte((id*31 + i*7) % 251)
	}
	return v
}

type counts struct {
	writes    int
	bytes     int64
	syncs     map[string]int
	opens     map[string]int
	unlinks   map[string]int
	truncates map[string]int
}

func newCounts() *counts {
	return &counts{
		syncs:     map[string]int{},
		opens:     map[string]int{},
		unlinks:   map[string]int{},
		truncates: map[string]int{},
	}
}

func (c *counts) totalSyncs() int {
	n := 0
	for _, v := range c.syncs {
		n += v
	}
	return n
}

func fileRole(path string) string {
	switch path {
	case dbName:
		return "db"
	case dbName + "-journal":
		return "journal"
	case dbName + "-wal":
		return "wal"
	case dbName + "-shm":
		return "shm"
	case ".":
		return "dir"
	}
	return ""
}

func (c *counts) add(s trace.Syscall) {
	role := fileRole(s.Path)
	if role == "" {
		return
	}
	switch trace.CanonicalName(s.Name) {
	case "pwrite64":
		c.writes++
		if s.Result > 0 {
			c.bytes += s.Result
		}
	case "sync":
		c.syncs[role]++
	case "open":
		c.opens[role]++
	case "unlink":
		c.unlinks[role]++
	case "ftruncate":
		c.truncates[role]++
	}
}

func roleList(m map[string]int) string {
	var roles []string
	for r, n := range m {
		if n > 0 {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		return "-"
	}
	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = fmt.Sprintf("%s:%d", r, m[r])
	}
	return strings.Join(parts, " ")
}

type measurement struct {
	commits []*counts
	all     *counts
	closeC  *counts
}

func (m *measurement) signature() string {
	return fmt.Sprintf("w=%d b=%d s=%d(%s) o=%s u=%s t=%s cw=%d cs=%d",
		m.all.writes, m.all.bytes, m.all.totalSyncs(), roleList(m.all.syncs),
		roleList(m.all.opens), roleList(m.all.unlinks), roleList(m.all.truncates),
		m.closeC.writes, m.closeC.totalSyncs())
}

func parseMarkedTrace(dir string, calls []trace.Syscall, wantCommits int) (*measurement, error) {
	rel := trace.RelativeTo(dir, calls)
	var segments [][]trace.Syscall
	var cur []trace.Syscall
	started := false
	for _, s := range rel {
		if trace.CanonicalName(s.Name) == "open" && s.Path == markerName {
			if started {
				segments = append(segments, cur)
			}
			started = true
			cur = nil
			continue
		}
		if started {
			cur = append(cur, s)
		}
	}
	if len(segments) != wantCommits+1 {
		return nil, fmt.Errorf("got %d marker segments, want %d commits + close", len(segments), wantCommits)
	}
	m := &measurement{all: newCounts(), closeC: newCounts()}
	for i, seg := range segments {
		c := newCounts()
		for _, s := range seg {
			c.add(s)
			if i < wantCommits {
				m.all.add(s)
			} else {
				m.closeC.add(s)
			}
		}
		if i < wantCommits {
			m.commits = append(m.commits, c)
		}
	}
	return m, nil
}

// runChildGo executes one workload cell in a child process with marker opens after CREATE, each commit, and close.
func runChildGo(args []string) {
	runtime.LockOSThread()
	if len(args) != 7 {
		fatal("child-go args", fmt.Errorf("got %v", args))
	}
	dir := args[0]
	journal := args[1]
	syncMode := args[2]
	total, err := strconv.Atoi(args[3])
	if err != nil {
		fatal("total", err)
	}
	batch, err := strconv.Atoi(args[4])
	if err != nil {
		fatal("batch", err)
	}
	autockpt, err := strconv.Atoi(args[5])
	if err != nil {
		fatal("autockpt", err)
	}
	holdReader := args[6] == "1"

	ctx := context.Background()
	o := sqlitex.DefaultOptions()
	o.File.JournalMode = journal
	o.Conn.Synchronous = syncMode
	o.Conn.WALAutocheckpoint = autockpt
	o.SingleConnection = true

	dbPath := filepath.Join(dir, dbName)
	db, err := sqlitex.Open(ctx, dbPath, o)
	if err != nil {
		fatal("child open", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB NOT NULL)"); err != nil {
		fatal("child create", err)
	}

	var readerDB *sqlitex.DB
	var readerTx interface{ Rollback() error }
	if holdReader {
		ro := o
		ro.Conn.WALAutocheckpoint = 0
		readerDB, err = sqlitex.Open(ctx, dbPath, ro)
		if err != nil {
			fatal("child reader open", err)
		}
		tx, err := readerDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			fatal("child reader begin", err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
			fatal("child reader select", err)
		}
		readerTx = tx
	}

	marker := filepath.Join(dir, markerName)
	touch := func() {
		if _, err := os.ReadFile(marker); err != nil {
			fatal("marker", err)
		}
	}
	touch()
	id := 0
	for range total / batch {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fatal("child begin", err)
		}
		for range batch {
			id++
			if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", id, value(id)); err != nil {
				fatal("child insert", err)
			}
		}
		if err := tx.Commit(); err != nil {
			fatal("child commit", err)
		}
		touch()
	}
	if readerTx != nil {
		_ = readerTx.Rollback()
		_ = readerDB.Close()
	}
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
	touch()
}

func measureGo(ctx context.Context, dir string, c cell, holdReader bool) (*measurement, error) {
	if err := os.WriteFile(filepath.Join(dir, markerName), nil, 0o644); err != nil {
		return nil, err
	}
	hr := "0"
	if holdReader {
		hr = "1"
	}
	cmd, err := trace.SelfCommand("child-go", dir, c.journal, c.syncMode,
		strconv.Itoa(c.total), strconv.Itoa(c.batch), strconv.Itoa(c.autockpt), hr)
	if err != nil {
		return nil, err
	}
	res, err := trace.Run(ctx, trace.Options{Command: cmd, Dir: dir, Syscalls: trace.DefaultSyscalls})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("child exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	return parseMarkedTrace(dir, res.Log.Calls, c.commits())
}

// measureCLI executes one 7.2 cell through the pinned C sqlite3 CLI under strace.
// Uses `.once commit-marker` + `.print m` to emit an unbuffered open syscall on commit-marker
// right after CREATE TABLE and after each commit; close runs from the last marker to process exit.
func measureCLI(ctx context.Context, cli sqlitex.CLI, dir string, c cell) (*measurement, error) {
	dbPath := filepath.Join(dir, dbName)
	markerPath := filepath.Join(dir, markerName)
	var b strings.Builder
	// Match sqlitex.DefaultOptions() exactly.
	fmt.Fprintf(&b, ".open --normal %s\n", dbPath)
	fmt.Fprintf(&b, "PRAGMA page_size=4096;\n")
	fmt.Fprintf(&b, "PRAGMA auto_vacuum=NONE;\n")
	fmt.Fprintf(&b, "PRAGMA encoding='UTF-8';\n")
	fmt.Fprintf(&b, "PRAGMA journal_mode=%s;\n", c.journal)
	fmt.Fprintf(&b, "PRAGMA application_id=%d;\n", sqlitex.CourseApplicationID)
	fmt.Fprintf(&b, "PRAGMA user_version=0;\n")
	fmt.Fprintf(&b, "PRAGMA synchronous=%s;\n", c.syncMode)
	fmt.Fprintf(&b, "PRAGMA foreign_keys=ON;\n")
	fmt.Fprintf(&b, "PRAGMA secure_delete=OFF;\n")
	fmt.Fprintf(&b, "PRAGMA cache_size=-2000;\n")
	fmt.Fprintf(&b, "PRAGMA wal_autocheckpoint=%d;\n", c.autockpt)
	fmt.Fprintf(&b, "PRAGMA busy_timeout=0;\n")
	fmt.Fprintf(&b, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB NOT NULL);\n")
	fmt.Fprintf(&b, ".once %s\n.print m\n", markerPath)
	id := 0
	for range c.commits() {
		b.WriteString("BEGIN IMMEDIATE;\n")
		for range c.batch {
			id++
			fmt.Fprintf(&b, "INSERT INTO t(id, v) VALUES(%d, X'%s');\n", id, strings.ToUpper(hex.EncodeToString(value(id))))
		}
		b.WriteString("COMMIT;\n")
		fmt.Fprintf(&b, ".once %s\n.print m\n", markerPath)
	}
	res, err := trace.Run(ctx, trace.Options{
		Command:  []string{cli.Path},
		Dir:      dir,
		Stdin:    strings.NewReader(b.String()),
		Syscalls: trace.DefaultSyscalls,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("cli exit %d: %s", res.ExitCode, res.Stderr)
	}
	// Parse markers: start after 1st open of commit-marker; each subsequent open closes one commit;
	// syscalls after the final marker belong to close at CLI EOF.
	rel := trace.RelativeTo(dir, res.Log.Calls)
	var segments [][]trace.Syscall
	var cur []trace.Syscall
	started := false
	for _, s := range rel {
		if trace.CanonicalName(s.Name) == "open" && s.Path == markerName {
			if started {
				segments = append(segments, cur)
			}
			started = true
			cur = nil
			continue
		}
		if started {
			cur = append(cur, s)
		}
	}
	// Remaining syscalls after the last commit marker are the close phase.
	segments = append(segments, cur)
	if len(segments) != c.commits()+1 {
		return nil, fmt.Errorf("cli got %d segments, want %d", len(segments), c.commits()+1)
	}
	m := &measurement{all: newCounts(), closeC: newCounts()}
	for i, seg := range segments {
		cnt := newCounts()
		for _, s := range seg {
			cnt.add(s)
			if i < c.commits() {
				m.all.add(s)
			} else {
				m.closeC.add(s)
			}
		}
		if i < c.commits() {
			m.commits = append(m.commits, cnt)
		}
	}
	return m, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child-go" {
		runChildGo(os.Args[2:])
		return
	}
	if !trace.Supported {
		fatal("platform", trace.ErrLinuxOnly)
	}
	ctx := context.Background()
	cli := sqlitex.MustLocateCLI(ctx)

	attackCOST01(ctx, cli)
	attackCOST02(ctx)
	attackCOST03(ctx)
	attackCOST04(ctx)
	attackCOST05(ctx)
	attackCOST06(ctx)
}

// COST-01: Recompute all 12 cells of 7.2 using the C CLI under strace and compare against 7.2 Go evidence.
func attackCOST01(ctx context.Context, cli sqlitex.CLI) {
	base := cleanDir(filepath.Join(scratchRoot, "cost01-cli"))
	allCells := cells12()
	results := make(map[cell]*measurement, len(allCells))
	errs := make(map[cell]error, len(allCells))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for _, c := range allCells {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dir := cleanDir(filepath.Join(base, fmt.Sprintf("%s-%s-%d", c.journal, strings.ToLower(c.syncMode), c.batch)))
			m, err := measureCLI(ctx, cli, dir, c)
			mu.Lock()
			results[c] = m
			errs[c] = err
			mu.Unlock()
		}()
	}
	wg.Wait()

	var mismatches []string
	matched := 0
	for _, c := range allCells {
		if errs[c] != nil {
			fatal("cost01 "+c.label(), errs[c])
		}
		m := results[c]
		want := evidence72[c]
		ok := m.all.writes == want.writes &&
			m.all.bytes == want.bytes &&
			m.all.totalSyncs() == want.syncs &&
			roleList(m.all.syncs) == want.syncBy &&
			roleList(m.all.opens) == want.opens &&
			roleList(m.all.unlinks) == want.unlinks &&
			roleList(m.all.truncates) == want.truncates &&
			m.closeC.writes == want.closeW &&
			m.closeC.totalSyncs() == want.closeSync
		if ok {
			matched++
		} else {
			mismatches = append(mismatches, fmt.Sprintf("%s got(%s) want(w=%d b=%d s=%d(%s) o=%s u=%s t=%s cw=%d cs=%d)",
				c.label(), m.signature(), want.writes, want.bytes, want.syncs, want.syncBy, want.opens, want.unlinks, want.truncates, want.closeW, want.closeSync))
		}
	}
	got := fmt.Sprintf("%d/12 CLI cells match 7.2 Go evidence exactly", matched)
	if len(mismatches) > 0 {
		got += "; diffs: " + strings.Join(mismatches, " | ")
	}
	emit("COST-01",
		"recompute all 12 cells of 7.2 using the pinned C sqlite3 CLI under strace",
		"all 12 CLI cells match 7.2 Go driver counts (pwrite, bytes, syncs by file, opens, unlinks, ftruncate, close)",
		got, len(mismatches) == 0)
}

// COST-02: synchronous=OFF across all 6 cells (delete/wal x 1x1000, 20x50, 1000x1).
func attackCOST02(ctx context.Context) {
	base := cleanDir(filepath.Join(scratchRoot, "cost02-off"))
	var offCells []cell
	for _, j := range []string{sqlitex.JournalDelete, sqlitex.JournalWAL} {
		for _, b := range []int{1000, 50, 1} {
			offCells = append(offCells, cell{j, sqlitex.SyncOff, 1000, b, 1000})
		}
	}
	results := map[cell]*measurement{}
	errs := map[cell]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for _, c := range offCells {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dir := cleanDir(filepath.Join(base, fmt.Sprintf("%s-off-%d", c.journal, c.batch)))
			m, err := measureGo(ctx, dir, c, false)
			mu.Lock()
			results[c] = m
			errs[c] = err
			mu.Unlock()
		}()
	}
	wg.Wait()

	allZeroSyncs := true
	var parts []string
	for _, c := range offCells {
		if errs[c] != nil {
			fatal("cost02 "+c.label(), errs[c])
		}
		m := results[c]
		if m.all.totalSyncs() != 0 || m.closeC.totalSyncs() != 0 {
			allZeroSyncs = false
		}
		parts = append(parts, fmt.Sprintf("%s/%dx%d:w=%d,b=%d,sync=%d,closeSync=%d",
			c.journal, c.commits(), c.batch, m.all.writes, m.all.bytes, m.all.totalSyncs(), m.closeC.totalSyncs()))
	}
	emit("COST-02",
		"run all 6 synchronous=OFF cells (delete/wal x 1x1000, 20x50, 1000x1) under strace",
		"0 sync syscalls during all commits (including WAL autocheckpoint+restart) and 0 syncs at close",
		strings.Join(parts, "; "), allZeroSyncs)
}

// COST-03: 20-run count stability across core cells including full wal/NORMAL 1000x1.
func attackCOST03(ctx context.Context) {
	base := cleanDir(filepath.Join(scratchRoot, "cost03-repeat20"))
	targetCells := []cell{
		{sqlitex.JournalDelete, sqlitex.SyncNormal, 1000, 50, 1000},
		{sqlitex.JournalDelete, sqlitex.SyncFull, 1000, 50, 1000},
		{sqlitex.JournalWAL, sqlitex.SyncFull, 1000, 50, 1000},
		{sqlitex.JournalWAL, sqlitex.SyncNormal, 1000, 1, 1000},
	}
	const runs = 20
	type job struct {
		c   cell
		run int
	}
	sigs := map[cell][]string{}
	for _, c := range targetCells {
		sigs[c] = make([]string, runs)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for _, c := range targetCells {
		for r := range runs {
			j := job{c, r}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				dir := cleanDir(filepath.Join(base, fmt.Sprintf("%s-%s-%d-r%02d", j.c.journal, strings.ToLower(j.c.syncMode), j.c.batch, j.run)))
				m, err := measureGo(ctx, dir, j.c, false)
				if err != nil {
					fatal("cost03 "+j.c.label(), err)
				}
				mu.Lock()
				sigs[j.c][j.run] = m.signature()
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	allStable := true
	var summary []string
	for _, c := range targetCells {
		uniq := map[string]int{}
		for _, s := range sigs[c] {
			uniq[s]++
		}
		want := evidence72[c]
		wantSig := fmt.Sprintf("w=%d b=%d s=%d(%s) o=%s u=%s t=%s cw=%d cs=%d",
			want.writes, want.bytes, want.syncs, want.syncBy, want.opens, want.unlinks, want.truncates, want.closeW, want.closeSync)
		if len(uniq) != 1 || sigs[c][0] != wantSig {
			allStable = false
		}
		summary = append(summary, fmt.Sprintf("%s:%d/%d identical(%d unique)", c.label(), uniq[wantSig], runs, len(uniq)))
	}
	emit("COST-03",
		"20 independent traced runs each of delete/NORMAL 20x50, delete/FULL 20x50, wal/FULL 20x50, and wal/NORMAL 1000x1",
		"all 20 runs per cell produce identical syscall counts matching 7.2 evidence",
		strings.Join(summary, "; "), allStable)
}

// COST-04: WAL/NORMAL sync count depends on wal_autocheckpoint (0 vs 100 vs 1000).
func attackCOST04(ctx context.Context) {
	base := cleanDir(filepath.Join(scratchRoot, "cost04-autockpt"))
	c0 := cell{sqlitex.JournalWAL, sqlitex.SyncNormal, 1000, 1, 0}
	c100 := cell{sqlitex.JournalWAL, sqlitex.SyncNormal, 1000, 1, 100}

	m0, err := measureGo(ctx, cleanDir(filepath.Join(base, "ckpt0")), c0, false)
	if err != nil {
		fatal("cost04 ckpt0", err)
	}
	m100, err := measureGo(ctx, cleanDir(filepath.Join(base, "ckpt100")), c100, false)
	if err != nil {
		fatal("cost04 ckpt100", err)
	}
	// Check: autockpt=0 has 0 commit syncs; autockpt=100 has multiple checkpoint+restart syncs (~30).
	ckpts100, restarts100 := 0, 0
	for _, cm := range m100.commits {
		switch roleList(cm.syncs) {
		case "db:1 wal:1":
			ckpts100++
		case "wal:1":
			restarts100++
		}
	}
	held := m0.all.totalSyncs() == 0 && m0.closeC.totalSyncs() == 2 &&
		ckpts100 >= 9 && restarts100 >= 9 && m100.all.totalSyncs() == 2*ckpts100+restarts100
	emit("COST-04",
		"wal/NORMAL 1000x1 with wal_autocheckpoint=0 vs wal_autocheckpoint=100",
		"autockpt=0 gives 0 commit syncs (2 at close); autockpt=100 gives 3 syncs per checkpoint+restart cycle (~30 syncs)",
		fmt.Sprintf("ckpt0:commitSyncs=%d,closeSyncs=%d; ckpt100:commitSyncs=%d(%s,ckpts=%d,restarts=%d),closeSyncs=%d",
			m0.all.totalSyncs(), m0.closeC.totalSyncs(), m100.all.totalSyncs(), roleList(m100.all.syncs), ckpts100, restarts100, m100.closeC.totalSyncs()),
		held)
}

// COST-05: synchronous=EXTRA (5 syncs/commit) and PERSIST/TRUNCATE journal reuse.
func attackCOST05(ctx context.Context) {
	base := cleanDir(filepath.Join(scratchRoot, "cost05-modes"))
	extraCell := cell{sqlitex.JournalDelete, sqlitex.SyncExtra, 1000, 50, 1000}
	persistCell := cell{sqlitex.JournalPersist, sqlitex.SyncNormal, 1000, 50, 1000}
	truncCell := cell{sqlitex.JournalTruncate, sqlitex.SyncFull, 1000, 50, 1000}

	mExtra, err := measureGo(ctx, cleanDir(filepath.Join(base, "extra")), extraCell, false)
	if err != nil {
		fatal("cost05 extra", err)
	}
	mPersist, err := measureGo(ctx, cleanDir(filepath.Join(base, "persist")), persistCell, false)
	if err != nil {
		fatal("cost05 persist", err)
	}
	mTrunc, err := measureGo(ctx, cleanDir(filepath.Join(base, "trunc")), truncCell, false)
	if err != nil {
		fatal("cost05 trunc", err)
	}

	// EXTRA: every commit should have 5 syncs [db:1 dir:2 journal:2] (100 total).
	extraOK := mExtra.all.totalSyncs() == 100 && roleList(mExtra.all.syncs) == "db:20 dir:40 journal:40"
	// PERSIST/NORMAL: commit 1 reuses the journal created during CREATE TABLE (or creates once), so commits 2..20 have 0 dir syncs and 0 unlinks!
	persistNoDirAfter1 := mPersist.all.unlinks["journal"] == 0
	for i := 1; i < len(mPersist.commits); i++ {
		if mPersist.commits[i].syncs["dir"] != 0 || mPersist.commits[i].totalSyncs() != 2 {
			persistNoDirAfter1 = false
		}
	}
	// TRUNCATE/FULL: commits 2..20 have 3 syncs (db:1 journal:2, 0 dir syncs) and 0 unlinks!
	truncNoDirAfter1 := mTrunc.all.unlinks["journal"] == 0
	for i := 1; i < len(mTrunc.commits); i++ {
		if mTrunc.commits[i].syncs["dir"] != 0 || mTrunc.commits[i].totalSyncs() != 3 {
			truncNoDirAfter1 = false
		}
	}
	emit("COST-05",
		"20x50 commits in delete/EXTRA, persist/NORMAL, and truncate/FULL under strace",
		"delete/EXTRA makes 5 syncs/commit (2 dir); persist/NORMAL and truncate/FULL eliminate dir syncs and unlinks after journal creation",
		fmt.Sprintf("delete/EXTRA:%d(%s); persist/NORMAL:%d(%s) unlinks=%s; truncate/FULL:%d(%s) unlinks=%s",
			mExtra.all.totalSyncs(), roleList(mExtra.all.syncs),
			mPersist.all.totalSyncs(), roleList(mPersist.all.syncs), roleList(mPersist.all.unlinks),
			mTrunc.all.totalSyncs(), roleList(mTrunc.all.syncs), roleList(mTrunc.all.unlinks)),
		extraOK && persistNoDirAfter1 && truncNoDirAfter1)
}

// COST-06: Held reader blocking checkpoint past wal_autocheckpoint under WAL/NORMAL.
// Attacks 7.2's claim that WAL/NORMAL only syncs on the commit that checkpoints and the commit that restarts.
func attackCOST06(ctx context.Context) {
	base := cleanDir(filepath.Join(scratchRoot, "cost06-pinned-reader"))
	c := cell{sqlitex.JournalWAL, sqlitex.SyncNormal, 100, 1, 50}
	m, err := measureGo(ctx, base, c, true)
	if err != nil {
		fatal("cost06", err)
	}
	commitsWithSync := 0
	var shapes []string
	seen := map[string]int{}
	for _, cm := range m.commits {
		if cm.totalSyncs() > 0 {
			commitsWithSync++
		}
		k := fmt.Sprintf("%d[%s]", cm.totalSyncs(), roleList(cm.syncs))
		if seen[k] == 0 {
			shapes = append(shapes, k)
		}
		seen[k]++
	}
	slices.Sort(shapes)
	var shapeParts []string
	for _, k := range shapes {
		shapeParts = append(shapeParts, fmt.Sprintf("%dx %s", seen[k], k))
	}
	// If the claim "WAL/NORMAL only syncs when checkpointing pages or restarting the WAL" holds even
	// when a reader blocks all progress, commitsWithSync should be <= 2. If SQLite syncs on EVERY
	// commit past autocheckpoint while blocked (or 0 times), report what really happens!
	held := commitsWithSync <= 2
	emit("COST-06",
		"wal/NORMAL 100x1 commits (autockpt=50) while a 2nd connection holds a read transaction pinning frame 0",
		"at most 2 commits sync (no checkpoint progress or WAL restart can occur while reader pins frame 0)",
		fmt.Sprintf("totalCommitSyncs=%d commitsWithSync=%d/100 shapes=(%s)", m.all.totalSyncs(), commitsWithSync, strings.Join(shapeParts, ", ")),
		held)
}
