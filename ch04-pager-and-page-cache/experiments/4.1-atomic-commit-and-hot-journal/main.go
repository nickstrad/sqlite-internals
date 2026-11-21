// Experiment 4.1 traces a multi-page rollback-journal commit through the
// kernel, decodes the live journal with internal/journalfile, kills the writer
// on entry to every write, sync, and unlink across synchronous=FULL/NORMAL/OFF,
// cross-checks the syscall stream against the pinned C CLI, demonstrates what
// happens when a hot journal is lost, and locates the exact commit point in
// DELETE, TRUNCATE, and PERSIST modes.
//
// Run from the repo root (Linux for strace parts; macOS runs parts A–C and an
// in-process fallback, or use docker compose run --rm lab):
//
//	go run ./ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/journalfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

const packagePath = "ch04-pager-and-page-cache/experiments/4.1-atomic-commit-and-hot-journal"

const (
	dbName      = "t.db"
	pageSize    = 4096
	markerBegin = "marker-begin"
	markerEnd   = "marker-end"
)

var (
	failures  []string
	stepFlag  = flag.Int("step", 3, "recipe step whose commit is traced and killed (the pre-step state is the step before it)")
	exactFlag = flag.Int("exact-step", 1, "second recipe step, killed at every point too, whose pre-step state has no freelist pages (0 skips it)")
	workers   = flag.Int("workers", 4, "kill runs executed in parallel")
)

// init pins the main goroutine to one OS thread so strace's per-thread syscall
// counter observes the child writer's SQLite syscalls in a deterministic order.
func init() {
	runtime.LockOSThread()
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

func rollbackRecipe() recipe.Recipe {
	return recipe.Default().WithJournalMode(sqlitex.JournalDelete)
}

func stepOptions(journalMode, synchronous string) sqlitex.Options {
	options := rollbackRecipe().SQLiteOptions()
	options.File.JournalMode = journalMode
	options.Conn.Synchronous = synchronous
	return options
}

// touch opens and reads an empty marker file so strace records an unambiguous
// open/openat boundary around the traced transaction without touching SQLite.
func touch(dir, name string) {
	if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
		fatal("marker", err)
	}
}

func childStep(dir, journalMode, synchronous string, k int) {
	ctx := context.Background()
	step, err := rollbackRecipe().Step(k)
	if err != nil {
		fatal("child step", err)
	}
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(journalMode, synchronous))
	if err != nil {
		fatal("child open", err)
	}
	touch(dir, markerBegin)
	if err := recipe.ApplyStep(ctx, db, step); err != nil {
		fatal("child apply", err)
	}
	touch(dir, markerEnd)
	if err := db.Close(); err != nil {
		fatal("child close", err)
	}
}

func copyFile(from, to string) {
	data, err := os.ReadFile(from)
	if err != nil {
		fatal("copy", err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		fatal("copy", err)
	}
}

func mkdir(dir string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
}

func prepareDir(dir, pre string) {
	mkdir(dir)
	if err := recipe.RemoveDatabase(filepath.Join(dir, dbName)); err != nil {
		fatal("remove", err)
	}
	copyFile(pre, filepath.Join(dir, dbName))
	for _, name := range []string{markerBegin, markerEnd} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			fatal("marker", err)
		}
	}
}

type states struct {
	step, preStep  int
	prePath        string
	pre, post      []byte
	prePages       int
	postPages      int
	hashes         []recipe.FileHashes
	preFreeLeaves  []uint32
	preCanon       string
	postCanon      string
	stepStatements recipe.Step
}

func newStates(ctx context.Context, dir string, r recipe.Recipe, k int, hashes []recipe.FileHashes) *states {
	mkdir(dir)
	s := &states{step: k, preStep: k - 1, hashes: hashes, prePath: filepath.Join(dir, "pre.db")}
	var err error
	s.stepStatements, err = r.Step(k)
	if err != nil {
		fatal("step", err)
	}
	if err := r.Build(ctx, s.prePath, k-1); err != nil {
		fatal("build pre-step", err)
	}
	postPath := filepath.Join(dir, "post.db")
	if err := r.Build(ctx, postPath, k); err != nil {
		fatal("build post-step", err)
	}
	if s.pre, err = os.ReadFile(s.prePath); err != nil {
		fatal("read pre", err)
	}
	if s.post, err = os.ReadFile(postPath); err != nil {
		fatal("read post", err)
	}
	s.prePages, s.postPages = len(s.pre)/pageSize, len(s.post)/pageSize
	// Collect pre-step freelist leaves: when the B-tree reuses a freelist page,
	// it fetches with PAGER_GET_NOCONTENT (src/btree.c:6740), which marks the
	// page in pInJournal without writing a journal record (src/pager.c:5645-5656).
	// A hot-journal rollback therefore cannot restore those unused bytes.
	file, err := dbfile.FromBytes(s.pre)
	if err != nil {
		fatal("parse pre", err)
	}
	census, err := file.Census()
	if err != nil {
		fatal("census pre", err)
	}
	for _, leaf := range census.FreelistLeaves {
		s.preFreeLeaves = append(s.preFreeLeaves, uint32(leaf))
	}
	slices.Sort(s.preFreeLeaves)
	// Canonical hashes (VACUUM INTO) provide rung R2 logical-to-byte equality
	// when a rolled-back file differs only inside reused freelist leaves.
	s.preCanon = canonicalHashOfFile(ctx, s.prePath)
	s.postCanon = canonicalHashOfFile(ctx, postPath)
	return s
}

func (s *states) classifyImage(image []byte) string {
	switch {
	case bytes.Equal(image, s.pre):
		return "pre-step bytes"
	case bytes.Equal(image, s.post):
		return "post-step bytes"
	}
	return fmt.Sprintf("torn: %d pages, %d differ from pre-step, %d differ from post-step", len(image)/pageSize, len(differingPages(image, s.pre)), len(differingPages(image, s.post)))
}

func (s *states) matchFile(path string) recipe.Match {
	h, err := recipe.HashFile(path)
	if err != nil {
		fatal("hash", err)
	}
	return recipe.MatchHashes(s.hashes, h)
}

type outcomeKind int

const (
	outcomePre outcomeKind = iota
	outcomePreFreelist
	outcomePost
	outcomeThird
	outcomeUnreadable
)

type outcome struct {
	kind     outcomeKind
	pages    []uint32
	match    recipe.Match
	canonPre bool
}

func (s *states) outcomeOf(path, canon string) outcome {
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read reopened", err)
	}
	o := outcome{match: s.matchFile(path), canonPre: canon != "" && canon == s.preCanon}
	switch {
	case canon == "":
		o.kind = outcomeUnreadable
	case bytes.Equal(image, s.pre):
		o.kind = outcomePre
	case bytes.Equal(image, s.post):
		o.kind = outcomePost
	default:
		o.pages = differingPages(image, s.pre)
		o.kind = outcomeThird
		// A rolled-back database whose file size and VACUUM INTO canonical hash
		// equal pre-step, and whose only differing pages are pre-step freelist
		// leaves, is logically identical to pre-step (PAGER_GET_NOCONTENT effect).
		if len(image) == len(s.pre) && o.canonPre && subset(o.pages, s.preFreeLeaves) {
			o.kind = outcomePreFreelist
		}
	}
	return o
}

func (o outcome) before() bool {
	return o.kind == outcomePre || o.kind == outcomePreFreelist
}

func (o outcome) letter() byte {
	switch o.kind {
	case outcomePre:
		return 'B'
	case outcomePreFreelist:
		return 'b'
	case outcomePost:
		return 'A'
	}
	return '?'
}

func (o outcome) describe(s *states) string {
	switch o.kind {
	case outcomePre:
		return fmt.Sprintf("pre-step, exact bytes (recipe step %d)", s.preStep)
	case outcomePreFreelist:
		return fmt.Sprintf("pre-step except page(s) %s, which are freelist leaves in the pre-step file (unused bytes); VACUUM INTO output equals the pre-step's", pageList(o.pages))
	case outcomePost:
		return fmt.Sprintf("post-step, exact bytes (recipe step %d)", s.step)
	case outcomeUnreadable:
		return "unreadable"
	}
	if o.match.Found() {
		return "a third state: recipe " + o.match.String()
	}
	return fmt.Sprintf("a third state: differs from pre-step in page(s) %s, matches no recipe state", pageList(o.pages))
}

func subset(pages, set []uint32) bool {
	for _, p := range pages {
		if !slices.Contains(set, p) {
			return false
		}
	}
	return true
}

func pageList(pages []uint32) string {
	parts := make([]string, len(pages))
	for i, p := range pages {
		parts[i] = strconv.Itoa(int(p))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func pageAt(image []byte, offset int) []byte {
	if offset >= len(image) {
		return nil
	}
	return image[offset:min(offset+pageSize, len(image))]
}

func differingPages(a, b []byte) []uint32 {
	var pages []uint32
	for i := 0; i < max(len(a), len(b)); i += pageSize {
		if !bytes.Equal(pageAt(a, i), pageAt(b, i)) {
			pages = append(pages, uint32(i/pageSize+1))
		}
	}
	return pages
}

func canonicalHashOfFile(ctx context.Context, path string) string {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open for VACUUM INTO", err)
	}
	defer db.Close()
	hash, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		fatal("VACUUM INTO", err)
	}
	return hash
}

func canonicalHash(ctx context.Context, db *sql.DB, path string) (string, error) {
	target := path + ".canonical"
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return "", err
	}
	defer os.Remove(target)
	return recipe.FileSHA256(target)
}

type op struct {
	call    trace.Syscall
	kind    string
	text    string
	ordinal int
}

func syncName(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

func openName(name string) bool {
	return name == "open" || name == "openat"
}

func unlinkName(name string) bool {
	return name == "unlink" || name == "unlinkat"
}

func killable(name string) bool {
	return name == "pwrite64" || name == "ftruncate" || syncName(name) || unlinkName(name)
}

func describe(call trace.Syscall) string {
	switch {
	case call.Name == "pwrite64" || call.Name == "pread64":
		text := fmt.Sprintf("%s %s off=%d len=%d", call.Name, call.Path, call.Offset, call.Length)
		if call.Result >= 0 && call.Result != call.Length && !call.Unfinished {
			text += fmt.Sprintf(" = %d", call.Result)
		}
		return text
	case syncName(call.Name):
		return "sync " + call.Path
	case unlinkName(call.Name):
		return "unlink " + call.Path
	case call.Name == "ftruncate":
		return fmt.Sprintf("ftruncate %s len=%d", call.Path, call.Length)
	case openName(call.Name):
		return "open " + call.Path
	case call.Name == "fcntl" && call.Lock != nil:
		return fmt.Sprintf("fcntl %s %s %s start=%d len=%d", call.Path, call.Lock.Command, call.Lock.Type, call.Lock.Start, call.Lock.Length)
	case call.Name == "fcntl":
		return fmt.Sprintf("fcntl %s %s", call.Path, strings.SplitN(call.Flags, ",", 2)[0])
	}
	return call.Name + " " + call.Path
}

// classify maps each traced syscall in the commit window to its pager role:
// 512-byte header write (writeJournalHdr, src/pager.c:1449-1571), 12-byte
// magic+count update (syncJournal, src/pager.c:4377-4410), 28-byte header zero
// (zeroJournalHdr, src/pager.c:1399-1432), record writes, db page writes,
// syncs (including the directory sync on a newly created journal), and journal
// finalization.
func classify(call trace.Syscall) string {
	journal := dbName + "-journal"
	switch {
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0 && call.Length == 12:
		return "journal-count"
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0 && call.Length == 28:
		return "journal-zero-header"
	case call.Name == "pwrite64" && call.Path == journal && call.Offset == 0:
		return "journal-header"
	case call.Name == "pwrite64" && call.Path == journal:
		return "journal-record"
	case call.Name == "pwrite64" && call.Path == dbName:
		return "db-page"
	case syncName(call.Name) && call.Path == journal:
		return "journal-sync"
	case syncName(call.Name) && call.Path == ".":
		return "dir-sync"
	case syncName(call.Name) && call.Path == dbName:
		return "db-sync"
	case unlinkName(call.Name) && call.Path == journal:
		return "journal-unlink"
	case call.Name == "ftruncate" && call.Path == journal:
		return "journal-truncate"
	case call.Name == "ftruncate" && call.Path == dbName:
		return "db-truncate"
	case openName(call.Name) && call.Path == markerEnd:
		return "step-returned"
	}
	return "other"
}

type window struct {
	tid     int
	ops     []op
	targets []op
}

// extractWindow isolates the writer thread's syscalls between marker-begin and
// marker-end while counting each syscall name's 1-based ordinal from process
// start, which strace's inject=...:when=ordinal flag requires.
func extractWindow(log *trace.StraceLog, dir string) (window, error) {
	w := window{tid: -1}
	beginPath := filepath.Join(dir, markerBegin)
	endPath := filepath.Join(dir, markerEnd)
	for _, call := range log.Calls {
		if openName(call.Name) && call.Path == beginPath {
			w.tid = call.PID
			break
		}
	}
	if w.tid < 0 {
		return w, errors.New("begin marker not found in the trace")
	}
	ordinals := map[string]int{}
	inside, ended := false, false
	for _, call := range log.Calls {
		if call.PID != w.tid || ended {
			continue
		}
		ordinals[call.Name]++
		if openName(call.Name) && call.Path == beginPath && !inside {
			inside = true
			continue
		}
		if !inside {
			continue
		}
		if openName(call.Name) && call.Path == endPath {
			relative := trace.RelativeTo(dir, []trace.Syscall{call})[0]
			w.targets = append(w.targets, op{call: relative, kind: classify(relative), text: describe(relative), ordinal: ordinals[call.Name]})
			ended = true
			continue
		}
		if !trace.UnderDir(dir)(call) || strings.HasPrefix(filepath.Base(call.Path), "marker-") {
			continue
		}
		relative := trace.RelativeTo(dir, []trace.Syscall{call})[0]
		o := op{call: relative, kind: classify(relative), text: describe(relative), ordinal: ordinals[call.Name]}
		w.ops = append(w.ops, o)
		if killable(call.Name) {
			w.targets = append(w.targets, o)
		}
	}
	if !ended {
		return w, errors.New("end marker not found in the trace")
	}
	return w, nil
}

func collapse(ops []op) string {
	var parts []string
	last, count := "", 0
	flush := func() {
		if last == "" {
			return
		}
		if count > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", last, count))
		} else {
			parts = append(parts, last)
		}
	}
	for _, o := range ops {
		if o.kind == "other" || o.kind == "step-returned" {
			continue
		}
		if o.kind == last {
			count++
			continue
		}
		flush()
		last, count = o.kind, 1
	}
	flush()
	return strings.Join(parts, ", ")
}

// recordTriplesWellFormed verifies that every journal-record write arrives as a
// (4, pageSize, 4) triple: page number, original page image, and checksum
// (pagerAddPageToRollbackJournal, src/pager.c:6062-6067).
func recordTriplesWellFormed(ops []op) bool {
	var lengths []int64
	for _, o := range ops {
		if o.kind == "journal-record" {
			lengths = append(lengths, int64(o.call.Length))
		}
	}
	if len(lengths)%3 != 0 {
		return false
	}
	for i := 0; i < len(lengths); i += 3 {
		if lengths[i] != 4 || lengths[i+1] != pageSize || lengths[i+2] != 4 {
			return false
		}
	}
	return true
}

func countKind(ops []op, kind string) int {
	n := 0
	for _, o := range ops {
		if o.kind == kind {
			n++
		}
	}
	return n
}

// expectedSequence builds the canonical commit syscall sequence for a given
// journal mode and synchronous level: FULL syncs the journal before and after
// writing magic+count (syncJournal, src/pager.c:4401-4420), NORMAL writes
// magic+count before its single journal sync, and OFF emits no syncs; TRUNCATE
// and PERSIST add a final journal sync at FULL (pager_end_transaction,
// src/pager.c:2086-2106).
func expectedSequence(journalMode, synchronous string, records, dbPages int) string {
	r := fmt.Sprintf("journal-record x%d", records)
	d := fmt.Sprintf("db-page x%d", dbPages)
	finish := map[string]string{
		sqlitex.JournalDelete:   "journal-unlink",
		sqlitex.JournalTruncate: "journal-truncate, journal-sync",
		sqlitex.JournalPersist:  "journal-zero-header, journal-sync",
	}[journalMode]
	switch synchronous {
	case sqlitex.SyncFull:
		return "journal-header, " + r + ", journal-sync, dir-sync, journal-count, journal-sync, " + d + ", db-sync, " + finish
	case sqlitex.SyncNormal:
		return "journal-header, " + r + ", journal-count, journal-sync, dir-sync, " + d + ", db-sync, " + finish
	}
	return "journal-header, " + r + ", " + d + ", " + finish
}

func syncTargets(ops []op) []string {
	var out []string
	for _, o := range ops {
		if syncName(o.call.Name) {
			out = append(out, o.call.Path)
		}
	}
	return out
}

func journalInfo(path string) (*journalfile.Journal, bool) {
	j, err := journalfile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		fatal("read journal", err)
	}
	return j, true
}

func journalSummary(j *journalfile.Journal, present bool) string {
	if !present {
		return "no -journal"
	}
	if j.Size == 0 {
		return "-journal 0 bytes (not hot)"
	}
	var b strings.Builder
	hot := "not hot"
	if j.LooksHot() {
		hot = "hot"
	}
	fmt.Fprintf(&b, "-journal %d bytes, first byte 0x%02x (%s)", j.Size, j.FirstByte, hot)
	if len(j.Segments) == 0 {
		return b.String()
	}
	h := j.Segments[0].Header
	fmt.Fprintf(&b, ", header %s", h.State)
	switch h.State {
	case journalfile.HeaderSynced:
		fmt.Fprintf(&b, " (record count %d)", h.RecordCount)
	case journalfile.HeaderNoSync:
		b.WriteString(" (record count 0xffffffff: read to end of file)")
	case journalfile.HeaderNotYetSynced:
		b.WriteString(" (magic and record count still zero)")
	case journalfile.HeaderZeroed:
		return b.String()
	}
	if h.State == journalfile.HeaderSynced || h.State == journalfile.HeaderNoSync || h.State == journalfile.HeaderNotYetSynced {
		fmt.Fprintf(&b, ", initial size %d pages, sector %d, page %d", h.InitialPages, h.SectorSize, h.PageSize)
	}
	fmt.Fprintf(&b, ", %d complete record(s) %s, %d checksum failure(s)", j.RecordsPresent(), recordPages(j), j.ChecksumFailures())
	if j.TrailingByte > 0 {
		fmt.Fprintf(&b, ", %d byte(s) of an incomplete record", j.TrailingByte)
	}
	return b.String()
}

func recordPages(j *journalfile.Journal) string {
	var pages []uint32
	for _, s := range j.Segments {
		for _, r := range s.Records {
			pages = append(pages, r.Page)
		}
	}
	return pageList(pages)
}

func playbackSummary(j *journalfile.Journal, present bool) string {
	if !present {
		return "nothing to roll back"
	}
	p := j.Playback()
	if !p.Hot {
		return "ignores it (not hot)"
	}
	if len(p.Pages) == 0 {
		return fmt.Sprintf("hot, but restores no page (%s)", p.StopReason)
	}
	return fmt.Sprintf("rolls back: writes pages %s from the journal, truncates to %d pages", pageList(p.Pages), p.TruncateTo)
}

type killResult struct {
	target        op
	landed        string
	landedOK      bool
	dbBefore      string
	dbBeforeImage []byte
	journal       *journalfile.Journal
	journalThere  bool
	integrity     string
	after         outcome
	journalAfter  string
	nonce         uint32
	duration      time.Duration
}

func killRun(ctx context.Context, dir string, s *states, journalMode, synchronous string, target op) *trace.Result {
	prepareDir(dir, s.prePath)
	command, err := trace.SelfCommand("child-step", dir, journalMode, synchronous, strconv.Itoa(s.step))
	if err != nil {
		fatal("self command", err)
	}
	run, err := trace.Run(ctx, trace.Options{Command: command, KillAt: &trace.KillAt{Syscalls: []string{target.call.Name}, When: target.ordinal}})
	if err != nil {
		fatal("strace kill run", err)
	}
	return run
}

// runKill runs one child process killed by SIGKILL on entry to target, inspects
// the resulting database and -journal files in plain Go before SQLite sees
// them, then reopens with sqlitex.Open to trigger sqlite3PagerSharedLock ->
// hasHotJournal -> pager_playback (src/pager.c:5300-5405) and classifies the
// recovered database.
func runKill(ctx context.Context, dir string, s *states, journalMode, synchronous string, target op) killResult {
	result := killResult{target: target}
	start := time.Now()
	run := killRun(ctx, dir, s, journalMode, synchronous, target)
	result.duration = time.Since(start)
	result.landed, result.landedOK = landedOn(run, dir, target)
	path := filepath.Join(dir, dbName)
	image, err := os.ReadFile(path)
	if err != nil {
		fatal("read killed db", err)
	}
	result.dbBeforeImage = image
	result.dbBefore = s.classifyImage(image)
	result.journal, result.journalThere = journalInfo(path + "-journal")
	if result.journalThere && len(result.journal.Segments) > 0 {
		result.nonce = result.journal.Segments[0].Header.Nonce
	}
	var canon string
	result.integrity, canon = reopen(ctx, path)
	result.after = s.outcomeOf(path, canon)
	after, there := journalInfo(path + "-journal")
	switch {
	case !there:
		result.journalAfter = "no -journal"
	case after.LooksHot():
		result.journalAfter = fmt.Sprintf("-journal still hot (%d bytes)", after.Size)
	default:
		result.journalAfter = fmt.Sprintf("cold -journal left (%d bytes, first byte 0x%02x)", after.Size, after.FirstByte)
	}
	return result
}

func reopen(ctx context.Context, path string) (string, string) {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		return "open failed: " + strings.ReplaceAll(err.Error(), path, filepath.Base(path)), ""
	}
	defer db.Close()
	integrity := integrityCheck(ctx, db.DB)
	canon, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		return integrity + "; VACUUM INTO failed: " + err.Error(), ""
	}
	return integrity, canon
}

func integrityCheck(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "integrity_check failed: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "integrity_check failed: " + err.Error()
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "integrity_check failed: " + err.Error()
	}
	return strings.Join(lines, " | ")
}

func runKills(ctx context.Context, base string, s *states, journalMode, synchronous string, targets []op) []killResult {
	results := make([]killResult, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(1, *workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = runKill(ctx, filepath.Join(base, fmt.Sprintf("kill-%02d", i+1)), s, journalMode, synchronous, targets[i])
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func baseline(ctx context.Context, dir string, s *states, journalMode, synchronous string) (window, []byte) {
	prepareDir(dir, s.prePath)
	command, err := trace.SelfCommand("child-step", dir, journalMode, synchronous, strconv.Itoa(s.step))
	if err != nil {
		fatal("self command", err)
	}
	run, err := trace.Run(ctx, trace.Options{Command: command})
	if err != nil {
		fatal("strace baseline", err)
	}
	if run.ExitCode != 0 {
		fatal("baseline child", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
	}
	w, err := extractWindow(run.Log, dir)
	if err != nil {
		fatal("baseline window", err)
	}
	image, err := os.ReadFile(filepath.Join(dir, dbName))
	if err != nil {
		fatal("read baseline db", err)
	}
	return w, image
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(x) + "'"
	}
	fatal("literal", fmt.Errorf("unsupported argument type %T", v))
	return ""
}

func renderStatement(st recipe.Statement) string {
	var b strings.Builder
	arg := 0
	for _, r := range st.SQL {
		if r == '?' {
			b.WriteString(sqlLiteral(st.Args[arg]))
			arg++
			continue
		}
		b.WriteRune(r)
	}
	if arg != len(st.Args) {
		fatal("render", fmt.Errorf("%d placeholders for %d arguments in %q", arg, len(st.Args), st.SQL))
	}
	return b.String()
}

func cliScript(dir, synchronous string, step recipe.Step) string {
	o := stepOptions(sqlitex.JournalDelete, synchronous)
	var b strings.Builder
	fmt.Fprintf(&b, "PRAGMA journal_mode=%s;\nPRAGMA synchronous=%s;\nPRAGMA foreign_keys=ON;\nPRAGMA secure_delete=ON;\nPRAGMA cache_size=%d;\nPRAGMA busy_timeout=%d;\n", sqlitex.JournalDelete, synchronous, o.Conn.CacheSize, o.Conn.BusyTimeout.Milliseconds())
	fmt.Fprintf(&b, ".read '%s'\nBEGIN IMMEDIATE;\n", filepath.Join(dir, markerBegin))
	for _, st := range step.Statements {
		b.WriteString(renderStatement(st))
		b.WriteString(";\n")
	}
	fmt.Fprintf(&b, "COMMIT;\n.read '%s'\n", filepath.Join(dir, markerEnd))
	return b.String()
}

// twinCheck replays the same recipe step as literal SQL through the pinned C
// sqlite3 CLI under strace and checks that its writes, syncs, and unlinks match
// the translated Go driver in file, offset, length, and order.
func twinCheck(ctx context.Context, cli sqlitex.CLI, dir string, s *states, synchronous string, goWindow window) {
	prepareDir(dir, s.prePath)
	command := cli.Command(ctx, sqlitex.CLIRun{}).Args
	command[0] = cli.Path
	path := filepath.Join(dir, dbName)
	run, err := trace.Run(ctx, trace.Options{Command: command, Dir: dir, Stdin: strings.NewReader(sqlitex.OpenScript(path, cliScript(dir, synchronous, s.stepStatements)))})
	if err != nil {
		fatal("strace CLI", err)
	}
	if run.ExitCode != 0 {
		fatal("CLI", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
	}
	cliWindow, err := extractWindow(run.Log, dir)
	if err != nil {
		fatal("CLI window", err)
	}
	goOps, cliOps := writesAndSyncs(goWindow.ops), writesAndSyncs(cliWindow.ops)
	same := slices.Equal(goOps, cliOps)
	if !same {
		for i := range max(len(goOps), len(cliOps)) {
			var g, c string
			if i < len(goOps) {
				g = goOps[i]
			}
			if i < len(cliOps) {
				c = cliOps[i]
			}
			if g != c {
				fmt.Printf("  twin #%d Go: %-45s CLI: %s\n", i+1, g, c)
			}
		}
	}
	verdict := "identical to the Go driver's"
	if !same {
		verdict = "DIFFERENT from the Go driver's"
	}
	fmt.Printf("twin check: the C CLI running the same step as literal SQL makes %d writes, syncs and unlinks, %s\n", len(cliOps), verdict)
	fmt.Printf("INFO: CLI sync syscall: %s\n", syncSyscallNames(cliWindow.ops))
	match := s.matchFile(path)
	check(same && match.Step == s.step && match.Exact, fmt.Sprintf("synchronous=%s: the C CLI makes the same writes, syncs and unlink in the same order and leaves the post-step bytes", synchronous))
}

func writesAndSyncs(ops []op) []string {
	var out []string
	for _, o := range ops {
		if killable(o.call.Name) {
			out = append(out, o.text)
		}
	}
	return out
}

func syncSyscallNames(ops []op) string {
	var names []string
	for _, o := range ops {
		if syncName(o.call.Name) && !slices.Contains(names, o.call.Name) {
			names = append(names, o.call.Name)
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ",")
}

type levelResult struct {
	level   string
	results []killResult
}

func checkBaseline(w window, image []byte, s *states, journalMode, synchronous string) {
	sequence := collapse(w.ops)
	records, dbPages := countKind(w.ops, "journal-record"), countKind(w.ops, "db-page")
	syncs := syncTargets(w.ops)
	fmt.Printf("sequence: %s\n", sequence)
	fmt.Printf("journaled pages %d (%d record writes), database page writes %d, syncs %d %v\n", records/3, records, dbPages, len(syncs), syncs)
	fmt.Printf("INFO: sync syscall used by the Go driver: %s\n", syncSyscallNames(w.ops))
	label := fmt.Sprintf("step %d, journal_mode=%s, synchronous=%s", s.step, journalMode, synchronous)
	check(sequence == expectedSequence(journalMode, synchronous, records, dbPages), label+": order is "+expectedSequence(journalMode, synchronous, records, dbPages))
	if journalMode == sqlitex.JournalDelete {
		wantSyncs := map[string]int{sqlitex.SyncFull: 4, sqlitex.SyncNormal: 3, sqlitex.SyncOff: 0}[synchronous]
		check(len(syncs) == wantSyncs, fmt.Sprintf("%s: %d syncs per commit (journal, directory and database syncs counted)", label, wantSyncs))
	}
	check(records > 0 && recordTriplesWellFormed(w.ops), label+": each journal record is three pwrite64 calls: page number (4 bytes), original page image (4096), checksum (4)")
	check(bytes.Equal(image, s.post), label+": the traced commit leaves exactly the post-step bytes")
}

func runLevel(ctx context.Context, base string, s *states, synchronous string, cli sqlitex.CLI, haveCLI, verbose bool) levelResult {
	levelDir := filepath.Join(base, strings.ToLower(synchronous))
	if verbose {
		fmt.Printf("\n== D. step %d, synchronous=%s: one commit traced, then killed at every write and sync ==\n", s.step, synchronous)
	} else {
		fmt.Printf("\n-- step %d, synchronous=%s --\n", s.step, synchronous)
	}
	w, image := baseline(ctx, filepath.Join(levelDir, "baseline"), s, sqlitex.JournalDelete, synchronous)
	if verbose {
		fmt.Printf("the commit's syscalls on the database directory (writer thread, between the step's start and its return):\n")
		for i, o := range w.ops {
			fmt.Printf("  %3d %-16s %s\n", i+1, o.kind, o.text)
		}
	}
	checkBaseline(w, image, s, sqlitex.JournalDelete, synchronous)
	if haveCLI && verbose {
		twinCheck(ctx, cli, filepath.Join(levelDir, "twin"), s, synchronous, w)
	}

	fmt.Printf("kill points: %d (every pwrite64, sync and unlink of the commit, then the first syscall after the step returned)\n", len(w.targets))
	started := time.Now()
	results := runKills(ctx, levelDir, s, sqlitex.JournalDelete, synchronous, w.targets)
	fmt.Printf("INFO: %d kill runs took %s with %d workers\n", len(results), time.Since(started).Round(time.Millisecond), max(1, *workers))
	unlinkIndex := -1
	for i, r := range results {
		if r.target.kind == "journal-unlink" {
			unlinkIndex = i
		}
	}
	allLanded, neverThird, integrityOK, coveredOK := true, true, true, true
	flipOK := unlinkIndex >= 0
	exactOnly := true
	beforeExact, beforeFreelist, after := 0, 0, 0
	for i, r := range results {
		if verbose {
			fmt.Printf("kill %02d on entry to %-38s [%s]\n", i+1, r.target.text, r.target.kind)
			fmt.Printf("    on disk:  database %s\n", r.dbBefore)
			fmt.Printf("              %s\n", journalSummary(r.journal, r.journalThere))
			fmt.Printf("              SQLite on reopen %s\n", playbackSummary(r.journal, r.journalThere))
			fmt.Printf("    reopened: %s; integrity_check %s; %s\n", r.after.describe(s), r.integrity, r.journalAfter)
		}
		if r.journalThere && len(r.journal.Segments) > 0 {
			fmt.Printf("INFO: step %d %s kill %02d journal nonce 0x%08x, run %s\n", s.step, synchronous, i+1, r.nonce, r.duration.Round(time.Millisecond))
		}
		if !r.landedOK {
			allLanded = false
			fmt.Printf("    kill %02d landed on %q, wanted %q\n", i+1, r.landed, r.target.text)
		}
		if r.integrity != "ok" {
			integrityOK = false
		}
		switch r.after.kind {
		case outcomePre:
			beforeExact++
		case outcomePreFreelist:
			beforeFreelist++
			exactOnly = false
		case outcomePost:
			after++
		default:
			neverThird, exactOnly = false, false
		}
		if (i <= unlinkIndex && !r.after.before()) || (i > unlinkIndex && r.after.kind != outcomePost) {
			flipOK = false
		}
		if i <= unlinkIndex && !covered(r, s) {
			coveredOK = false
			fmt.Printf("    kill %02d: the journal does not cover every changed page of the database file\n", i+1)
		}
	}
	label := fmt.Sprintf("step %d, synchronous=%s", s.step, synchronous)
	fmt.Printf("outcomes: %d pre-step exact, %d pre-step except reused freelist leaves, %d post-step exact, %d other\n", beforeExact, beforeFreelist, after, len(results)-beforeExact-beforeFreelist-after)
	check(allLanded, label+": every kill landed on the intended syscall (strace saw it unfinished and the child died of SIGKILL)")
	check(neverThird, label+": every reopened database is the pre-step or the post-step state, never a third (pre-step allows reused freelist-leaf pages, see above)")
	if len(s.preFreeLeaves) == 0 {
		check(exactOnly, label+": the pre-step file has no freelist, and every reopened database equals the pre-step or post-step bytes exactly")
	}
	check(flipOK, label+": every kill up to and including the journal unlink gives pre-step; the kill after it gives post-step (the unlink is the commit point)")
	check(integrityOK, label+": integrity_check is ok after every reopen")
	check(coveredOK, label+": for every kill up to the commit point, whenever the database file differs from pre-step before reopen, the journal is hot and its records cover every changed page except pages appended past the original size (removed by truncation) and reused freelist leaves")
	return levelResult{level: synchronous, results: results}
}

// covered verifies that whenever a pre-commit crash leaves modified bytes in
// the database file, the journal is hot and accounts for every changed page:
// appended pages (> prePages) are removed by truncation (src/pager.c:2952),
// existing modified pages are listed in Playback().Pages, and reused freelist
// leaves were fetched with PAGER_GET_NOCONTENT (src/pager.c:5645-5656).
func covered(r killResult, s *states) bool {
	changed := differingPages(r.dbBeforeImage, s.pre)
	if len(changed) == 0 {
		return true
	}
	if !r.journalThere {
		return false
	}
	p := r.journal.Playback()
	if !p.Hot {
		return false
	}
	for _, page := range changed {
		switch {
		case int(page) > s.prePages:
			if p.TruncateTo != uint32(s.prePages) {
				return false
			}
		case slices.Contains(p.Pages, page), slices.Contains(s.preFreeLeaves, page):
		default:
			return false
		}
	}
	return true
}

func printSummary(s *states, levels []levelResult) {
	fmt.Printf("\n== summary for step %d: reopened state per kill point, in commit order ==\n", s.step)
	for _, level := range levels {
		var line strings.Builder
		for _, r := range level.results {
			line.WriteByte(r.after.letter())
		}
		fmt.Printf("synchronous=%-6s %s\n", level.level, line.String())
	}
	fmt.Printf("(B = pre-step exact bytes, b = pre-step except reused freelist leaves, A = post-step exact bytes, ? = anything else; the last B or b is the kill on entry to the journal unlink)\n")
}

func journalOpsIn(calls []trace.Syscall) []string {
	var ops []string
	for _, call := range calls {
		if !strings.HasSuffix(call.Path, "-journal") {
			continue
		}
		if openName(call.Name) || unlinkName(call.Name) || syncName(call.Name) || call.Name == "pwrite64" {
			ops = append(ops, describe(call))
		}
	}
	return ops
}

func walWritesIn(calls []trace.Syscall) int {
	n := 0
	for _, call := range calls {
		if strings.HasSuffix(call.Path, "-wal") && call.Name == "pwrite64" {
			n++
		}
	}
	return n
}

func segmentsByMarkers(log *trace.StraceLog, dir, prefix string, count int) [][]trace.Syscall {
	segments := make([][]trace.Syscall, count)
	current := -1
	for _, call := range log.Calls {
		if openName(call.Name) && strings.HasPrefix(call.Path, filepath.Join(dir, prefix)) {
			n, err := strconv.Atoi(strings.TrimPrefix(call.Path, filepath.Join(dir, prefix)))
			if err == nil {
				current = n - 1
			}
			continue
		}
		if current >= 0 && current < count && trace.UnderDir(dir)(call) {
			segments[current] = append(segments[current], trace.RelativeTo(dir, []trace.Syscall{call})[0])
		}
	}
	return segments
}

// partA shows where a WAL-default platform still meets rollback mode: new
// databases default to journal_mode=delete (header bytes 18-19 = 1,1), and
// switching a new file to WAL—including inside sqlitex.Open—writes page 1
// through a rollback -journal before WAL mode takes effect.
func partA(ctx context.Context, base string) {
	fmt.Printf("\n== A. where a WAL-default platform meets rollback mode ==\n")
	dir := filepath.Join(base, "a")
	mkdir(dir)
	path := filepath.Join(dir, "plain.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fatal("open plain", err)
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		fatal("journal_mode", err)
	}
	fmt.Printf("a new file opened by the driver with no settings reports journal_mode=%s\n", mode)
	check(mode == sqlitex.JournalDelete, "a new database starts in rollback-journal mode (journal_mode=delete) until something switches it")
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		fatal("create", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	header := readHeaderBytes(path)
	fmt.Printf("after its first CREATE TABLE: header bytes 18,19 = %d,%d (1 = rollback journal)\n", header[18], header[19])
	check(header[18] == 1 && header[19] == 1, "the file records rollback mode in header bytes 18 and 19 (value 1)")
	db, err = sql.Open("sqlite", path)
	if err != nil {
		fatal("reopen plain", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=wal").Scan(&mode); err != nil {
		fatal("switch", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	header = readHeaderBytes(path)
	db, err = sql.Open("sqlite", path)
	if err != nil {
		fatal("reopen plain", err)
	}
	var again string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&again); err != nil {
		fatal("journal_mode", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	fmt.Printf("after PRAGMA journal_mode=wal: header bytes 18,19 = %d,%d; a fresh connection reports journal_mode=%s\n", header[18], header[19], again)
	check(header[18] == 2 && header[19] == 2 && again == sqlitex.JournalWAL, "switching to WAL rewrites bytes 18 and 19 to 2, and the mode persists in the file")

	if !trace.Supported {
		return
	}
	cases := []struct {
		role   string
		labels []string
	}{
		{"plain", []string{"CREATE TABLE (driver defaults, first write)"}},
		{"switch", []string{"PRAGMA journal_mode=wal on the new empty file", "CREATE TABLE after the switch"}},
		{"platform", []string{"sqlitex.Open of a new file with journal_mode=wal", "CREATE TABLE after it"}},
	}
	journalBySegment := map[string][][]string{}
	walBySegment := map[string][]int{}
	for _, c := range cases {
		target := filepath.Join(dir, c.role+".db")
		if err := recipe.RemoveDatabase(target); err != nil {
			fatal("remove", err)
		}
		for i := 1; i <= len(c.labels)+1; i++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-marker-%d", c.role, i)), nil, 0o644); err != nil {
				fatal("marker", err)
			}
		}
		command, err := trace.SelfCommand("child-create", c.role, target)
		if err != nil {
			fatal("self command", err)
		}
		run, err := trace.Run(ctx, trace.Options{Command: command})
		if err != nil {
			fatal("strace create", err)
		}
		if run.ExitCode != 0 {
			fatal("create child", fmt.Errorf("exit %d: %s", run.ExitCode, run.Stderr))
		}
		segments := segmentsByMarkers(run.Log, dir, c.role+"-marker-", len(c.labels))
		for i, label := range c.labels {
			ops := journalOpsIn(segments[i])
			wal := walWritesIn(segments[i])
			journalBySegment[c.role] = append(journalBySegment[c.role], ops)
			walBySegment[c.role] = append(walBySegment[c.role], wal)
			fmt.Printf("%s: -journal %v; -wal writes %d\n", label, ops, wal)
		}
	}
	usesJournal := func(ops []string) bool {
		hasPrefix := func(prefix string) bool {
			return slices.ContainsFunc(ops, func(o string) bool { return strings.HasPrefix(o, prefix) })
		}
		return len(ops) >= 2 && strings.HasPrefix(ops[0], "open ") && strings.HasPrefix(ops[len(ops)-1], "unlink ") && hasPrefix("pwrite64 ") && hasPrefix("sync ")
	}
	check(usesJournal(journalBySegment["plain"][0]), "a file created with driver defaults commits its first transaction through a -journal (opened, written, synced, unlinked)")
	check(usesJournal(journalBySegment["switch"][0]) && walBySegment["switch"][0] == 0, "the switch to WAL is itself a rollback-mode commit: it writes page 1 through a -journal")
	check(len(journalBySegment["switch"][1]) == 0 && walBySegment["switch"][1] > 0, "after the switch, commits go to the -wal and no -journal is opened")
	check(usesJournal(journalBySegment["platform"][0]) && len(journalBySegment["platform"][1]) == 0, "a WAL database created by the platform opener (sqlitex.Open) is also born through one -journal commit, then never uses one")
}

func readHeaderBytes(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read header", err)
	}
	if len(data) < 100 {
		fatal("read header", fmt.Errorf("file has %d bytes", len(data)))
	}
	return data[:100]
}

func childCreate(role, path string) {
	ctx := context.Background()
	dir := filepath.Dir(path)
	mark := func(i int) { touch(dir, fmt.Sprintf("%s-marker-%d", role, i)) }
	exec := func(db *sql.DB, statement string) {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
	switch role {
	case "plain", "switch":
		db, err := sql.Open("sqlite", path)
		if err != nil {
			fatal("plain open", err)
		}
		db.SetMaxOpenConns(1)
		mark(1)
		if role == "switch" {
			exec(db, "PRAGMA journal_mode=wal")
			mark(2)
			exec(db, "CREATE TABLE t(x)")
			mark(3)
		} else {
			exec(db, "CREATE TABLE t(x)")
			mark(2)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	case "platform":
		mark(1)
		db, err := sqlitex.Open(ctx, path, sqlitex.DefaultOptions())
		if err != nil {
			fatal("platform open", err)
		}
		mark(2)
		exec(db.DB, "CREATE TABLE t(x)")
		mark(3)
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
	default:
		fatal("child-create", fmt.Errorf("unknown role %q", role))
	}
}

// partC inspects the rollback journal of an open, uncommitted transaction at
// synchronous=FULL using internal/journalfile. Reading -journal while the
// connection is open is safe because SQLite locks only the main database file.
func partC(ctx context.Context, base string, s *states) {
	fmt.Printf("\n== C. the journal of an open transaction, read before COMMIT ==\n")
	dir := filepath.Join(base, "c")
	prepareDir(dir, s.prePath)
	db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	if err := s.stepStatements.ExecIn(ctx, tx); err != nil {
		fatal("exec", err)
	}
	j, present := journalInfo(filepath.Join(dir, dbName+"-journal"))
	if !present {
		fatal("journal", errors.New("no -journal during the transaction"))
	}
	fmt.Printf("every statement of step %d executed, COMMIT not yet issued: %s\n", s.step, journalSummary(j, present))
	allOriginal := j.RecordsPresent() > 0
	for _, segment := range j.Segments {
		for _, r := range segment.Records {
			same := bytes.Equal(pageAt(s.pre, int(r.Page-1)*pageSize), r.Image)
			allOriginal = allOriginal && same && r.ChecksumOK()
			role := "b-tree or overflow page"
			if r.Page == 1 {
				role = "header and schema root"
			}
			image := "equals the pre-step page"
			if !same {
				image = "DIFFERS from the pre-step page"
			}
			fmt.Printf("  record at offset %6d: page %3d (%s), checksum %s, image %s\n", r.Offset, r.Page, role, okWord(r.ChecksumOK()), image)
		}
	}
	check(allOriginal, "every journal record holds the page's original (pre-step) image with a valid checksum")
	check(j.Segments[0].Header.State == journalfile.HeaderNotYetSynced && !j.LooksHot(), "before COMMIT (synchronous=FULL) the header's magic and record count are still zero, so the journal is not hot yet")
	check(j.Segments[0].Header.InitialPages == uint32(s.prePages), fmt.Sprintf("the header records the database size before the transaction (%d pages)", s.prePages))
	fmt.Printf("pre-step freelist leaf pages (never journaled when reused): %s\n", pageList(s.preFreeLeaves))
	if err := tx.Rollback(); err != nil {
		fatal("rollback", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	match := s.matchFile(filepath.Join(dir, dbName))
	fmt.Printf("after ROLLBACK and close: %s\n", map[bool]string{true: "pre-step exact bytes", false: "NOT the pre-step bytes"}[match.Step == s.preStep && match.Exact])
	check(match.Step == s.preStep && match.Exact, "a ROLLBACK by the live connection restores the pre-step bytes exactly (the reused freelist pages were never written to the file)")
	check(recipe.CheckAtRest(filepath.Join(dir, dbName)) == nil, "no -journal remains after ROLLBACK and close")
}

func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "BAD"
}

func damageReport(ctx context.Context, path string, s *states) (string, outcome) {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		return "open failed: " + strings.ReplaceAll(err.Error(), path, filepath.Base(path)), s.outcomeOf(path, "")
	}
	integrity := integrityCheck(ctx, db.DB)
	counts := tableCounts(ctx, db.DB)
	canon, err := canonicalHash(ctx, db.DB, path)
	if err != nil {
		canon = ""
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return fmt.Sprintf("integrity_check: %s; %s", integrity, counts), s.outcomeOf(path, canon)
}

func tableCounts(ctx context.Context, db *sql.DB) string {
	var parts []string
	var userVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		parts = append(parts, "user_version error: "+err.Error())
	} else {
		parts = append(parts, fmt.Sprintf("user_version=%d", userVersion))
	}
	for _, table := range []string{"accounts", "documents", "events"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			parts = append(parts, fmt.Sprintf("count(%s) error: %v", table, err))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", table, n))
	}
	return strings.Join(parts, " ")
}

func countsOf(ctx context.Context, path string) string {
	db, err := sqlitex.Open(ctx, path, stepOptions(sqlitex.JournalDelete, sqlitex.SyncFull))
	if err != nil {
		fatal("open", err)
	}
	defer db.Close()
	return tableCounts(ctx, db.DB)
}

func recreateKilled(ctx context.Context, dir string, s *states, target op) {
	run := killRun(ctx, dir, s, sqlitex.JournalDelete, sqlitex.SyncFull, target)
	if landed, ok := landedOn(run, dir, target); !ok {
		fatal("kill", fmt.Errorf("child was not killed on %q (ordinal %d): killed=%q exit %d landed %q", target.text, target.ordinal, run.Killed, run.ExitCode, landed))
	}
}

// landedOn confirms strace killed the child with SIGKILL on entry to the exact
// intended syscall by matching both formatted syscall text and per-PID ordinal
// (strace counts --inject when= per thread). Tracking ordinals per PID also
// handles logs where a killed pwrite64 appears unfinished on a second thread.
func landedOn(run *trace.Result, dir string, target op) (string, bool) {
	landed, onTarget := "", false
	ordinals := map[int]int{}
	for _, call := range run.Log.Calls {
		if call.Name != target.call.Name {
			continue
		}
		ordinals[call.PID]++
		if !call.Unfinished || !trace.UnderDir(dir)(call) {
			continue
		}
		text := describe(trace.RelativeTo(dir, []trace.Syscall{call})[0])
		if text == target.text && ordinals[call.PID] == target.ordinal {
			landed, onTarget = text, true
		} else if !onTarget {
			landed = text
		}
	}
	return landed, run.Killed == "SIGKILL" && onTarget
}

// partE demonstrates three failure cases of losing a hot journal: deleting it
// mid-write, copying the database file alone versus together with -journal, and
// deleting it after all database writes completed (silently committing a
// transaction whose COMMIT never returned).
func partE(ctx context.Context, base string, s *states, fullTargets []op) {
	fmt.Printf("\n== E. failure cases: losing the hot journal (step %d, synchronous=FULL) ==\n", s.step)
	fmt.Printf("contents: pre-step %s; post-step %s\n", countsOf(ctx, s.prePath), countsOf(ctx, filepath.Join(filepath.Dir(s.prePath), "post.db")))
	var dbWrites []op
	var dbSync op
	for _, t := range fullTargets {
		switch t.kind {
		case "db-page":
			dbWrites = append(dbWrites, t)
		case "db-sync":
			dbSync = t
		}
	}
	if len(dbWrites) < 2 || dbSync.kind == "" {
		fatal("failure cases", errors.New("the synchronous=FULL commit has too few database writes"))
	}
	middle := dbWrites[len(dbWrites)/2]
	fmt.Printf("crash state: killed on entry to %s (database write %d of %d)\n", middle.text, len(dbWrites)/2+1, len(dbWrites))

	deleted := filepath.Join(base, "e-deleted")
	recreateKilled(ctx, deleted, s, middle)
	image, err := os.ReadFile(filepath.Join(deleted, dbName))
	if err != nil {
		fatal("read", err)
	}
	j, present := journalInfo(filepath.Join(deleted, dbName+"-journal"))
	fmt.Printf("on disk: database %s\n         %s\n", s.classifyImage(image), journalSummary(j, present))

	copied := filepath.Join(base, "e-copied")
	original := filepath.Join(base, "e-with-journal")
	mkdir(copied)
	mkdir(original)
	copyFile(filepath.Join(deleted, dbName), filepath.Join(copied, dbName))
	copyFile(filepath.Join(deleted, dbName), filepath.Join(original, dbName))
	copyFile(filepath.Join(deleted, dbName+"-journal"), filepath.Join(original, dbName+"-journal"))

	if err := os.Remove(filepath.Join(deleted, dbName+"-journal")); err != nil {
		fatal("delete journal", err)
	}
	report, deletedOutcome := damageReport(ctx, filepath.Join(deleted, dbName), s)
	fmt.Printf("1. -journal deleted, then reopened: %s\n   state: %s\n", report, deletedOutcome.describe(s))
	check(deletedOutcome.kind == outcomeUnreadable || deletedOutcome.kind == outcomeThird, "deleting the hot journal before reopening leaves a damaged database that is neither the pre-step nor the post-step state")

	copyReport, copyOutcome := damageReport(ctx, filepath.Join(copied, dbName), s)
	fmt.Printf("2. the database file copied without its -journal, copy opened: %s\n   state: %s\n", copyReport, copyOutcome.describe(s))
	check(copyOutcome.kind == deletedOutcome.kind && copyReport == report && !copyOutcome.before(), "a copy made without the -journal is the same damaged database")
	originalReport, originalOutcome := damageReport(ctx, filepath.Join(original, dbName), s)
	fmt.Printf("   the same crashed bytes copied together with their -journal, opened: %s\n   state: %s\n", originalReport, originalOutcome.describe(s))
	check(originalOutcome.before(), "the same crashed file copied together with its -journal rolls back to pre-step")

	late := filepath.Join(base, "e-late")
	recreateKilled(ctx, late, s, dbSync)
	image, err = os.ReadFile(filepath.Join(late, dbName))
	if err != nil {
		fatal("read", err)
	}
	fmt.Printf("3. killed on entry to %s (every database page written, journal still hot): database %s\n", dbSync.text, s.classifyImage(image))
	if err := os.Remove(filepath.Join(late, dbName+"-journal")); err != nil {
		fatal("delete journal", err)
	}
	lateReport, lateOutcome := damageReport(ctx, filepath.Join(late, dbName), s)
	fmt.Printf("   -journal deleted, then reopened: %s\n   state: %s\n", lateReport, lateOutcome.describe(s))
	check(lateOutcome.kind == outcomePost, "deleting the journal after the last database write turns a commit that never returned into a committed one (post-step)")
}

// partF locates the exact commit-point syscall for DELETE (unlink), TRUNCATE
// (ftruncate to 0), and PERSIST (28-byte header zero write) by killing on entry
// to that syscall and on the syscall immediately after it
// (pager_end_transaction, src/pager.c:2086-2122).
func partF(ctx context.Context, base string, s *states) {
	fmt.Printf("\n== F. the commit point in DELETE, TRUNCATE and PERSIST journal modes (step %d, synchronous=FULL) ==\n", s.step)
	type row struct{ mode, point, at, after, left string }
	var rows []row
	for _, mode := range []string{sqlitex.JournalDelete, sqlitex.JournalTruncate, sqlitex.JournalPersist} {
		dir := filepath.Join(base, "f-"+mode)
		fmt.Printf("-- journal_mode=%s --\n", mode)
		w, image := baseline(ctx, filepath.Join(dir, "baseline"), s, mode, sqlitex.SyncFull)
		checkBaseline(w, image, s, mode, sqlitex.SyncFull)
		left, there := journalInfo(filepath.Join(dir, "baseline", dbName+"-journal"))
		leftText := journalSummary(left, there)
		commitKind := map[string]string{sqlitex.JournalDelete: "journal-unlink", sqlitex.JournalTruncate: "journal-truncate", sqlitex.JournalPersist: "journal-zero-header"}[mode]
		var commitPoint, next op
		for i, t := range w.targets {
			if t.kind == commitKind && i+1 < len(w.targets) {
				commitPoint, next = t, w.targets[i+1]
				break
			}
		}
		if commitPoint.kind == "" {
			check(false, mode+": commit point found in the trace")
			continue
		}
		at := runKill(ctx, filepath.Join(dir, "at"), s, mode, sqlitex.SyncFull, commitPoint)
		after := runKill(ctx, filepath.Join(dir, "after"), s, mode, sqlitex.SyncFull, next)
		rows = append(rows, row{mode, commitPoint.text, at.after.shortName(), after.after.shortName() + " (kill on " + next.kind + ")", leftText})
		check(at.landedOK && after.landedOK && at.after.before() && after.after.kind == outcomePost, mode+": a kill on entry to the commit point gives pre-step, the next kill gives post-step")
		if mode != sqlitex.JournalDelete {
			check(there && !left.LooksHot(), mode+": the journal file stays beside the database after a clean commit, and it is not hot")
		}
	}
	fmt.Printf("%-9s %-34s %-26s %-44s %s\n", "mode", "commit point (syscall)", "killed on entry", "killed on the next syscall", "-journal after a clean commit")
	for _, r := range rows {
		fmt.Printf("%-9s %-34s %-26s %-44s %s\n", r.mode, r.point, r.at, r.after, r.left)
	}
}

func (o outcome) shortName() string {
	switch o.kind {
	case outcomePre:
		return "before (pre-step)"
	case outcomePreFreelist:
		return "before (pre-step, logical)"
	case outcomePost:
		return "after (post-step)"
	}
	return "neither"
}

func portableLevels(ctx context.Context, base string, s *states) {
	for _, level := range []string{sqlitex.SyncFull, sqlitex.SyncNormal, sqlitex.SyncOff} {
		dir := filepath.Join(base, "portable-"+strings.ToLower(level))
		prepareDir(dir, s.prePath)
		db, err := sqlitex.Open(ctx, filepath.Join(dir, dbName), stepOptions(sqlitex.JournalDelete, level))
		if err != nil {
			fatal("open", err)
		}
		if err := recipe.ApplyStep(ctx, db, s.stepStatements); err != nil {
			fatal("apply", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		match := s.matchFile(filepath.Join(dir, dbName))
		check(match.Step == s.step && match.Exact, fmt.Sprintf("step %d, synchronous=%s: the step committed in process leaves the post-step bytes", s.step, level))
	}
}

func describeStates(s *states) {
	fmt.Println(s.stepStatements.Summary())
	fmt.Printf("pre-step state (after step %d): %d pages, sha256 %s, freelist leaves %s\n", s.preStep, s.prePages, s.hashes[s.preStep].Exact[:16], pageList(s.preFreeLeaves))
	fmt.Printf("post-step state (after step %d): %d pages, sha256 %s\n", s.step, s.postPages, s.hashes[s.step].Exact[:16])
	preMatch, postMatch := s.matchFile(s.prePath), s.matchFile(filepath.Join(filepath.Dir(s.prePath), "post.db"))
	check(preMatch.Step == s.preStep && preMatch.Exact && postMatch.Step == s.step && postMatch.Exact, fmt.Sprintf("the pre-step and post-step files equal the recipe's hashes for steps %d and %d", s.preStep, s.step))
}

func main() {
	if len(os.Args) == 6 && os.Args[1] == "child-step" {
		k, err := strconv.Atoi(os.Args[5])
		if err != nil {
			fatal("child step", err)
		}
		childStep(os.Args[2], os.Args[3], os.Args[4], k)
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "child-create" {
		childCreate(os.Args[2], os.Args[3])
		return
	}
	flag.Parse()
	ctx := context.Background()
	started := time.Now()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("4.1")
	base, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(base))
	var cli sqlitex.CLI
	haveCLI := false
	if trace.Supported {
		located, cliErr := sqlitex.LocateCLI(ctx)
		cli, haveCLI = located, cliErr == nil
		if haveCLI {
			fmt.Println(cli.PinLine())
		} else {
			fmt.Printf("sqlite3 CLI: unavailable: %v\n", cliErr)
			failures = append(failures, "the pinned sqlite3 CLI is available")
		}
	}

	partA(ctx, base)

	r := rollbackRecipe()
	for _, k := range []int{*stepFlag, *exactFlag} {
		if k != 0 && (k < 1 || k > r.Steps) {
			fatal("step", fmt.Errorf("steps must be 1..%d, got %d", r.Steps, k))
		}
	}
	fmt.Printf("\n== B. the recipe step ==\n")
	fmt.Printf("recipe: %s\n", r)
	hashes, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	main := newStates(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *stepFlag)), r, *stepFlag, hashes)
	describeStates(main)

	partC(ctx, base, main)

	if !trace.Supported {
		trace.PrintLinuxOnly("the strace of the commit, the kill at every write and sync, the failure cases and the commit-point table", packagePath)
		portableLevels(ctx, base, main)
		finish(started)
		return
	}

	levels := []string{sqlitex.SyncFull, sqlitex.SyncNormal, sqlitex.SyncOff}
	var mainResults []levelResult
	var fullTargets []op
	for _, level := range levels {
		result := runLevel(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *stepFlag)), main, level, cli, haveCLI, true)
		mainResults = append(mainResults, result)
		if level == sqlitex.SyncFull {
			for _, kill := range result.results {
				fullTargets = append(fullTargets, kill.target)
			}
		}
	}
	printSummary(main, mainResults)

	if *exactFlag != 0 {
		fmt.Printf("\n== D2. a step whose pre-step file has no freelist: step %d, every kill point, all three levels ==\n", *exactFlag)
		exact := newStates(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *exactFlag)), r, *exactFlag, hashes)
		describeStates(exact)
		var exactResults []levelResult
		for _, level := range levels {
			exactResults = append(exactResults, runLevel(ctx, filepath.Join(base, fmt.Sprintf("step-%d", *exactFlag)), exact, level, cli, false, false))
		}
		printSummary(exact, exactResults)
	}

	partE(ctx, base, main, fullTargets)
	partF(ctx, base, main)
	finish(started)
}

func finish(started time.Time) {
	fmt.Printf("INFO: runtime %s\n", time.Since(started).Round(100*time.Millisecond))
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("ATOMIC COMMIT FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("ATOMIC COMMIT OK")
}

func displayPath(path string) string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		return path
	}
	return filepath.ToSlash(relative)
}
