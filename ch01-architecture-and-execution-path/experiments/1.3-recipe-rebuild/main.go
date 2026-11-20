// Experiment 1.3 proves Rebuild Ladder rung R0: running the seeded recipe
// twice in different directories under different names produces byte-identical
// database files at every step. It then tests six sources of variation to show
// which determinism rules are required and where SQLite's behavior differs
// between WAL and rollback modes.
//
// Run from the repo root (portable; no strace needed):
//
//	go run ./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize     = 4096
	variantBase  = 6
	maxRowid     = "9223372036854775807"
	secondsTries = 3
)

var failures []string

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

type fileDiff struct {
	Equal          bool
	SizeA, SizeB   int
	DifferingPages []int
	Page1Bytes     []int
}

func (d fileDiff) String() string {
	if d.Equal {
		return "identical"
	}
	if len(d.DifferingPages) == 0 {
		return fmt.Sprintf("differ: sizes %d and %d bytes", d.SizeA, d.SizeB)
	}
	text := fmt.Sprintf("differ: first differing page %d, %d page(s) differ %v", d.DifferingPages[0], len(d.DifferingPages), limitPages(d.DifferingPages))
	if d.SizeA != d.SizeB {
		text += fmt.Sprintf(", sizes %d and %d pages", d.SizeA/pageSize, d.SizeB/pageSize)
	}
	if len(d.Page1Bytes) > 0 {
		text += fmt.Sprintf(", page 1 bytes %v", d.Page1Bytes)
	}
	return text
}

func limitPages(pages []int) string {
	if len(pages) <= 8 {
		return fmt.Sprint(pages)
	}
	return fmt.Sprintf("%v...", pages[:8])
}

// compareFiles compares two closed database files page by page (1-based) and
// records the differing byte offsets inside page 1 when page 1 differs.
func compareFiles(a, b string) fileDiff {
	dataA, err := os.ReadFile(a)
	if err != nil {
		fatal("read", err)
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		fatal("read", err)
	}
	return diffImages(dataA, dataB)
}

func pageSlice(data []byte, p int) []byte {
	start := p * pageSize
	if start >= len(data) {
		return nil
	}
	return data[start:min((p+1)*pageSize, len(data))]
}

func diffImages(dataA, dataB []byte) fileDiff {
	d := fileDiff{Equal: bytes.Equal(dataA, dataB), SizeA: len(dataA), SizeB: len(dataB)}
	pages := (max(len(dataA), len(dataB)) + pageSize - 1) / pageSize
	for p := range pages {
		pageA, pageB := pageSlice(dataA, p), pageSlice(dataB, p)
		if bytes.Equal(pageA, pageB) {
			continue
		}
		d.DifferingPages = append(d.DifferingPages, p+1)
		if p == 0 {
			for i := range min(len(pageA), len(pageB)) {
				if pageA[i] != pageB[i] {
					d.Page1Bytes = append(d.Page1Bytes, i)
				}
			}
		}
	}
	return d
}

// onlyChangeCounter reports whether two equal-sized files differ exclusively
// inside header bytes 24..27 (file change counter) and 92..95
// (version-valid-for), written by pager_write_changecounter (src/pager.c:3130-3143).
func onlyChangeCounter(d fileDiff) bool {
	if d.Equal || d.SizeA != d.SizeB || !slices.Equal(d.DifferingPages, []int{1}) {
		return false
	}
	return onlyCounterBytes(d.Page1Bytes)
}

func onlyCounterBytes(offsets []int) bool {
	for _, offset := range offsets {
		inCounter := false
		for _, span := range recipe.ChangeCounterRanges {
			if offset >= span[0] && offset < span[1] {
				inCounter = true
			}
		}
		if !inCounter {
			return false
		}
	}
	return true
}

func hashOf(path string) recipe.FileHashes {
	h, err := recipe.HashFile(path)
	if err != nil {
		fatal("hash", err)
	}
	return h
}

func pages(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		fatal("stat", err)
	}
	return int(info.Size() / pageSize)
}

func rebuild(ctx context.Context, r recipe.Recipe, path string) []recipe.FileHashes {
	var hashes []recipe.FileHashes
	err := r.BuildEach(ctx, path, r.Steps, func(k int) error {
		hashes = append(hashes, hashOf(path))
		return nil
	})
	if err != nil {
		fatal("build "+path, err)
	}
	return hashes
}

func applyStatementsInSession(ctx context.Context, r recipe.Recipe, path string, pragmas []string, statements ...string) {
	db, err := r.Open(ctx, path)
	if err != nil {
		fatal("open", err)
	}
	// Pin a single *sql.Conn so any PRAGMAs apply to the exact connection that
	// runs the transaction.
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	for _, pragma := range pragmas {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			fatal(pragma, err)
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			fatal(statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := conn.Close(); err != nil {
		fatal("conn close", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("at rest", err)
	}
}

func buildBase(ctx context.Context, r recipe.Recipe, path string) {
	if err := r.Build(ctx, path, variantBase); err != nil {
		fatal("base", err)
	}
}

type variant struct {
	name      string
	rule      string
	pragmas   []string
	statement []string
}

func runTwice(ctx context.Context, r recipe.Recipe, dir string, v variant) fileDiff {
	a := filepath.Join(dir, v.name+"-run1.db")
	b := filepath.Join(dir, v.name+"-run2.db")
	for _, path := range []string{a, b} {
		buildBase(ctx, r, path)
		applyStatementsInSession(ctx, r, path, v.pragmas, v.statement...)
	}
	d := compareFiles(a, b)
	fmt.Printf("INFO: %s sha256 run 1 %s, run 2 %s\n", v.name, hashOf(a).Exact, hashOf(b).Exact)
	return d
}

// timestampVariant demonstrates both sides of current_timestamp's one-second
// resolution: two runs inside the same wall-clock second produce identical
// files, while a third run after the second advances differs.
func timestampVariant(ctx context.Context, r recipe.Recipe, dir string) (sameSecond, nextSecond fileDiff) {
	statement := "INSERT INTO events(id, account_id, kind, amount, note) VALUES(900002, 1, 'fee', 0, current_timestamp)"
	a := filepath.Join(dir, "timestamp-run1.db")
	b := filepath.Join(dir, "timestamp-run2.db")
	c := filepath.Join(dir, "timestamp-run3.db")
	for attempt := range secondsTries {
		for _, path := range []string{a, b, c} {
			buildBase(ctx, r, path)
		}
		waitForNextSecond()
		started := time.Now().Unix()
		applyStatementsInSession(ctx, r, a, nil, statement)
		applyStatementsInSession(ctx, r, b, nil, statement)
		if time.Now().Unix() != started {
			fmt.Printf("INFO: timestamp attempt %d crossed a second boundary; retrying\n", attempt+1)
			continue
		}
		waitForNextSecond()
		applyStatementsInSession(ctx, r, c, nil, statement)
		fmt.Printf("INFO: timestamp sha256 run 1 %s, run 2 %s, run 3 %s\n", hashOf(a).Exact, hashOf(b).Exact, hashOf(c).Exact)
		return compareFiles(a, b), compareFiles(a, c)
	}
	fatal("timestamp variant", fmt.Errorf("could not run two inserts inside one wall-clock second in %d attempts", secondsTries))
	return fileDiff{}, fileDiff{}
}

func waitForNextSecond() {
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now) + 5*time.Millisecond)
}

// mergedWithNext selects steps 2+3 and 4+5 to merge into single transactions:
// step 3 deletes rows written in step 2, exposing stale unzeroed bytes when
// secure_delete=OFF.
var mergedWithNext = []int{2, 4}

func batchVariant(ctx context.Context, r recipe.Recipe, dir, secureDelete string) (fileDiff, unusedReport) {
	plan, err := r.WithSteps(variantBase).Plan()
	if err != nil {
		fatal("plan", err)
	}
	options := r.SQLiteOptions()
	label := fmt.Sprintf("%s-secure-delete-%s", r.JournalMode, strings.ToLower(secureDelete))
	a := filepath.Join(dir, "batches-"+label+"-one-per-step.db")
	b := filepath.Join(dir, "batches-"+label+"-merged.db")
	for _, path := range []string{a, b} {
		if err := recipe.RemoveDatabase(path); err != nil {
			fatal("remove", err)
		}
		db, err := sqlitex.Open(ctx, path, options)
		if err != nil {
			fatal("open", err)
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			fatal("conn", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA secure_delete="+secureDelete); err != nil {
			fatal("secure_delete", err)
		}
		merge := path == b
		for i := 0; i < len(plan); i++ {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				fatal("begin", err)
			}
			if err := plan[i].ExecIn(ctx, tx); err != nil {
				fatal("step", err)
			}
			if merge && slices.Contains(mergedWithNext, i) {
				i++
				if err := plan[i].ExecIn(ctx, tx); err != nil {
					fatal("step", err)
				}
			}
			if err := tx.Commit(); err != nil {
				fatal("commit", err)
			}
		}
		if err := conn.Close(); err != nil {
			fatal("conn close", err)
		}
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		if err := recipe.CheckAtRest(path); err != nil {
			fatal("at rest", err)
		}
	}
	fmt.Printf("INFO: batches %s sha256 one-per-step %s, merged %s\n", label, hashOf(a).Exact, hashOf(b).Exact)
	d := compareFiles(a, b)
	return d, classifyDifferences(a, b, d)
}

func be16(data []byte, offset int) int {
	return int(data[offset])<<8 | int(data[offset+1])
}

func be32(data []byte, offset int) int {
	return int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
}

type freelist struct {
	trunkLeafCounts map[int]int
	leaves          map[int]bool
}

func readFreelist(data []byte) (freelist, error) {
	fl := freelist{trunkLeafCounts: map[int]int{}, leaves: map[int]bool{}}
	if len(data) < pageSize || len(data)%pageSize != 0 {
		return fl, fmt.Errorf("file size %d is not a whole number of %d-byte pages", len(data), pageSize)
	}
	if headerPageSize := be16(data, 16); headerPageSize != pageSize {
		return fl, fmt.Errorf("header page size %d, want %d", headerPageSize, pageSize)
	}
	pageCount := len(data) / pageSize
	maxLeaves := (pageSize - 8) / 4
	for trunk := be32(data, 32); trunk != 0; {
		if trunk < 1 || trunk > pageCount {
			return fl, fmt.Errorf("freelist trunk page %d outside 1..%d", trunk, pageCount)
		}
		if _, seen := fl.trunkLeafCounts[trunk]; seen || fl.leaves[trunk] {
			return fl, fmt.Errorf("freelist page %d listed twice", trunk)
		}
		base := (trunk - 1) * pageSize
		leafCount := be32(data, base+4)
		if leafCount > maxLeaves {
			return fl, fmt.Errorf("freelist trunk page %d claims %d leaves, at most %d fit", trunk, leafCount, maxLeaves)
		}
		fl.trunkLeafCounts[trunk] = leafCount
		for i := range leafCount {
			leaf := be32(data, base+8+4*i)
			if leaf < 1 || leaf > pageCount {
				return fl, fmt.Errorf("freelist leaf page %d outside 1..%d", leaf, pageCount)
			}
			if _, seen := fl.trunkLeafCounts[leaf]; seen || fl.leaves[leaf] {
				return fl, fmt.Errorf("freelist page %d listed twice", leaf)
			}
			fl.leaves[leaf] = true
		}
		trunk = be32(data, base)
	}
	if total := be32(data, 36); total != len(fl.trunkLeafCounts)+len(fl.leaves) {
		return fl, fmt.Errorf("header counts %d freelist pages, the chain holds %d", total, len(fl.trunkLeafCounts)+len(fl.leaves))
	}
	return fl, nil
}

func (fl freelist) contains(page int) bool {
	_, trunk := fl.trunkLeafCounts[page]
	return trunk || fl.leaves[page]
}

func isBtreePageType(t byte) bool {
	return t == 0x02 || t == 0x05 || t == 0x0a || t == 0x0d
}

func unusedBytes(data []byte, page int, fl freelist) ([]bool, string, error) {
	unused := make([]bool, pageSize)
	if fl.leaves[page] {
		for i := range unused {
			unused[i] = true
		}
		return unused, "freelist leaf page", nil
	}
	if leafCount, ok := fl.trunkLeafCounts[page]; ok {
		for i := 8 + 4*leafCount; i < pageSize; i++ {
			unused[i] = true
		}
		return unused, "freelist trunk page, bytes after the leaf list", nil
	}
	content := data[(page-1)*pageSize : page*pageSize]
	header := 0
	if page == 1 {
		header = 100
	}
	if !isBtreePageType(content[header]) {
		return unused, fmt.Sprintf("not a b-tree or freelist page (byte %d is 0x%02x)", header, content[header]), nil
	}
	headerSize := 8
	if content[header] == 0x02 || content[header] == 0x05 {
		headerSize = 12
	}
	cellPointersEnd := header + headerSize + 2*be16(content, header+3)
	contentStart := be16(content, header+5)
	if contentStart == 0 {
		contentStart = 65536
	}
	contentStart = min(contentStart, pageSize)
	if cellPointersEnd > contentStart {
		return nil, "", fmt.Errorf("page %d: cell pointers end at %d, after the content area starts at %d", page, cellPointersEnd, contentStart)
	}
	for i := cellPointersEnd; i < contentStart; i++ {
		unused[i] = true
	}
	previousEnd := contentStart
	for block := be16(content, header+1); block != 0; block = be16(content, block) {
		if block < previousEnd || block+4 > pageSize {
			return nil, "", fmt.Errorf("page %d: freeblock at %d is out of order or outside the content area", page, block)
		}
		size := be16(content, block+2)
		if size < 4 || block+size > pageSize {
			return nil, "", fmt.Errorf("page %d: freeblock at %d has size %d", page, block, size)
		}
		for i := block + 4; i < block+size; i++ {
			unused[i] = true
		}
		previousEnd = block + size
	}
	return unused, "b-tree page, unallocated space and freeblocks", nil
}

type unusedReport struct {
	OnlyUnused    bool
	Kinds         []string
	LeafPages     []int
	FreelistPages []int
}

func classifyDifferences(a, b string, d fileDiff) unusedReport {
	dataA, err := os.ReadFile(a)
	if err != nil {
		fatal("read", err)
	}
	dataB, err := os.ReadFile(b)
	if err != nil {
		fatal("read", err)
	}
	return classifyImages(dataA, dataB, d)
}

func classifyImages(dataA, dataB []byte, d fileDiff) unusedReport {
	report := unusedReport{OnlyUnused: len(dataA) == len(dataB) && len(d.DifferingPages) > 0}
	if len(dataA) != len(dataB) {
		return report
	}
	freeA, errA := readFreelist(dataA)
	freeB, errB := readFreelist(dataB)
	if errA != nil || errB != nil {
		report.OnlyUnused = false
		report.Kinds = append(report.Kinds, fmt.Sprintf("freelist unreadable: %v / %v", errA, errB))
		return report
	}
	for _, page := range d.DifferingPages {
		unusedA, kindA, errA := unusedBytes(dataA, page, freeA)
		unusedB, kindB, errB := unusedBytes(dataB, page, freeB)
		if errA != nil || errB != nil {
			report.OnlyUnused = false
			report.Kinds = append(report.Kinds, fmt.Sprintf("page %d: unparseable (%v / %v)", page, errA, errB))
			continue
		}
		base := (page - 1) * pageSize
		count, inUse := 0, 0
		for i := range pageSize {
			if dataA[base+i] != dataB[base+i] {
				count++
				if !unusedA[i] || !unusedB[i] {
					inUse++
					report.OnlyUnused = false
				}
			}
		}
		if freeA.leaves[page] && freeB.leaves[page] {
			report.LeafPages = append(report.LeafPages, page)
		}
		if freeA.contains(page) && freeB.contains(page) {
			report.FreelistPages = append(report.FreelistPages, page)
		}
		kind := kindA
		if kindB != kindA {
			kind = kindA + " / " + kindB
		}
		if inUse > 0 {
			kind += fmt.Sprintf("; %d of these bytes are outside unused space", inUse)
		}
		report.Kinds = append(report.Kinds, fmt.Sprintf("page %d: %d bytes (%s)", page, count, kind))
	}
	return report
}

// sessionVariant compares building steps 0..variantBase with one connection
// session per step against building the exact same steps on one long-lived
// connection. Under WAL mode only the header change counter differs because
// dbFileVers is refreshed on page-1 read, not on WAL commit (src/pager.c:3109-3110).
func sessionVariant(ctx context.Context, r recipe.Recipe, dir string) fileDiff {
	canonical := filepath.Join(dir, "sessions-"+r.JournalMode+"-one-per-step.db")
	if err := r.Build(ctx, canonical, variantBase); err != nil {
		fatal("build", err)
	}
	longLived := filepath.Join(dir, "sessions-"+r.JournalMode+"-one-for-all.db")
	if err := recipe.RemoveDatabase(longLived); err != nil {
		fatal("remove", err)
	}
	db, err := r.Open(ctx, longLived)
	if err != nil {
		fatal("open", err)
	}
	for k := 0; k <= variantBase; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal("step", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return compareFiles(canonical, longLived)
}

func main() {
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("1.3")
	dir, err := filepath.EvalSymlinks(work)
	if err != nil {
		fatal("work dir", err)
	}
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))

	r := recipe.Default()
	fmt.Printf("recipe: %s (index created at step %d, settings: %s; %s)\n", r, recipe.IndexStep, r.SQLiteOptions().File, r.SQLiteOptions().Conn)
	plan, err := r.Plan()
	if err != nil {
		fatal("plan", err)
	}
	for _, s := range plan {
		fmt.Printf("  %s\n", s.Summary())
	}

	fmt.Println("\n== R0: rebuild the recipe twice ==")
	if err := os.MkdirAll(filepath.Join(dir, "run-1"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "run-2", "elsewhere"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	first := filepath.Join(dir, "run-1", "recipe.db")
	second := filepath.Join(dir, "run-2", "elsewhere", "another-name.db")
	started := time.Now()
	firstHashes := rebuild(ctx, r, first)
	fmt.Printf("INFO: one build of %d steps took %s\n", r.Steps, time.Since(started).Round(time.Millisecond))
	secondHashes := rebuild(ctx, r, second)
	fmt.Printf("run 1 (run-1/recipe.db): %d pages, sha256 %s\n", pages(first), hashOf(first).Exact)
	fmt.Printf("run 2 (run-2/elsewhere/another-name.db): %d pages, sha256 %s\n", pages(second), hashOf(second).Exact)
	check(hashOf(first) == hashOf(second), "two builds of the recipe in different directories under different names are byte-identical")
	fmt.Println("per-step state hashes (sha256 of the closed file after step k):")
	for k, h := range firstHashes {
		fmt.Printf("  step %2d %s\n", k, h.Exact)
	}
	check(slices.Equal(firstHashes, secondHashes), fmt.Sprintf("the per-step hash lists of the two runs are identical (%d states)", len(firstHashes)))
	distinct := map[string]bool{}
	for _, h := range firstHashes {
		distinct[h.Exact] = true
	}
	check(len(distinct) == len(firstHashes), "every step leaves a different file, so a hash names exactly one step")
	cached, err := r.StepHashes(ctx)
	if err != nil {
		fatal("step hashes", err)
	}
	check(slices.Equal(cached, firstHashes), "recipe.StepHashes (the cached list later chapters use) equals the list just built")
	middle := filepath.Join(dir, "run-1", "to-step-12.db")
	if err := r.Build(ctx, middle, 12); err != nil {
		fatal("build", err)
	}
	match, err := r.MatchStep(ctx, middle)
	if err != nil {
		fatal("match", err)
	}
	fmt.Printf("lookup: a database built to step 12 is %s\n", match)
	check(match == recipe.Match{Step: 12, Exact: true}, "recipe.MatchStep names the step a database equals")

	deleteRecipe := r.WithJournalMode(sqlitex.JournalDelete)
	deleteA := filepath.Join(dir, "run-1", "recipe-delete.db")
	deleteB := filepath.Join(dir, "run-2", "recipe-delete.db")
	deleteHashesA := rebuild(ctx, deleteRecipe, deleteA)
	deleteHashesB := rebuild(ctx, deleteRecipe, deleteB)
	fmt.Printf("same recipe in rollback mode (journal_mode=delete): sha256 %s\n", hashOf(deleteA).Exact)
	check(slices.Equal(deleteHashesA, deleteHashesB), "the rollback-mode recipe also rebuilds byte for byte at every step")
	modes := compareFiles(first, deleteA)
	fmt.Printf("WAL build vs rollback build of the same recipe: %s\n", modes)

	fmt.Printf("\n== broken variants: recipe to step %d, then one extra transaction, each run twice into fresh files ==\n", variantBase)
	randomDiff := runTwice(ctx, r, dir, variant{
		name:      "random",
		rule:      "no SQL random()",
		statement: []string{"INSERT INTO events(id, account_id, kind, amount, note) VALUES(900001, 1, 'fee', random(), 'random')"},
	})
	fmt.Printf("1. SQL random() in a value: %s\n", randomDiff)
	check(!randomDiff.Equal, "SQL random() makes two runs differ")

	sameSecond, nextSecond := timestampVariant(ctx, r, dir)
	fmt.Printf("2. current_timestamp in a value, two runs inside one wall-clock second: %s\n", sameSecond)
	fmt.Printf("   current_timestamp in a value, third run one second later: %s\n", nextSecond)
	check(sameSecond.Equal && !nextSecond.Equal, "current_timestamp makes runs differ once the wall clock's second changes (its resolution is one second, so two fast runs can still match)")

	unordered := "INSERT INTO archive(account_id, amount) SELECT account_id, amount FROM events WHERE amount > 0"
	ordered := unordered + " ORDER BY id"
	createArchive := "CREATE TABLE archive(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, amount INTEGER NOT NULL)"
	unorderedSame := runTwice(ctx, r, dir, variant{name: "unordered", statement: []string{createArchive, unordered}})
	fmt.Printf("3. INSERT ... SELECT without ORDER BY (rowids from scan order), same plan twice: %s\n", unorderedSame)
	unorderedPlan := runTwiceWithPlanChange(ctx, r, dir, "unordered-plan", createArchive, unordered)
	fmt.Printf("   the same statement, run 2 with a different scan order (PRAGMA reverse_unordered_selects=ON): %s\n", unorderedPlan)
	orderedPlan := runTwiceWithPlanChange(ctx, r, dir, "ordered-plan", createArchive, ordered)
	fmt.Printf("   with ORDER BY id, run 2 with reverse_unordered_selects=ON: %s\n", orderedPlan)
	check(unorderedSame.Equal, "FINDING: an unordered INSERT ... SELECT run twice on the same build with the same plan gives identical files")
	check(!unorderedPlan.Equal && orderedPlan.Equal, "an unordered INSERT ... SELECT changes the file when the scan order changes; ORDER BY removes the dependence")

	batchesWAL, _ := batchVariant(ctx, r, dir, "ON")
	fmt.Printf("4. steps 0-%d on one connection, steps 2+3 and 4+5 each committed as one transaction instead of two:\n", variantBase)
	fmt.Printf("   WAL mode, recipe settings (secure_delete=ON): %s\n", batchesWAL)
	batchesWALOff, unusedWALOff := batchVariant(ctx, r, dir, "OFF")
	fmt.Printf("   WAL mode, secure_delete=OFF: %s\n", batchesWALOff)
	for _, kind := range unusedWALOff.Kinds {
		fmt.Printf("     %s\n", kind)
	}
	batchesDelete, _ := batchVariant(ctx, deleteRecipe, dir, "ON")
	fmt.Printf("   rollback mode, secure_delete=ON: %s\n", batchesDelete)
	check(batchesWAL.Equal, "FINDING: in WAL mode on one connection with secure_delete=ON, different transaction batches give identical files")
	check(!batchesWALOff.Equal && unusedWALOff.OnlyUnused, "with secure_delete=OFF different batches differ, and only in bytes no cell uses (stale content left in unallocated space, freeblocks or freelist pages)")
	check(onlyChangeCounter(batchesDelete), "in rollback mode different batches differ, and with secure_delete=ON only in the change counter (it counts commits)")
	fastVsOn, fastVsOnUnused := secureDeleteFastVariant(ctx, r, dir)
	fmt.Printf("   WAL mode, one transaction per step, secure_delete=FAST vs secure_delete=ON: %s\n", fastVsOn)
	for _, kind := range fastVsOnUnused.Kinds {
		fmt.Printf("     %s\n", kind)
	}
	check(!fastVsOn.Equal && fastVsOnUnused.OnlyUnused && slices.Equal(fastVsOnUnused.FreelistPages, fastVsOn.DifferingPages), "secure_delete=FAST is not ON: on the same history FAST leaves old bytes in the unused space of freelist pages that ON zeroes")

	rowidDiff := runTwice(ctx, r, dir, variant{
		name: "rowid",
		statement: []string{
			"CREATE TABLE tickets(id INTEGER PRIMARY KEY, label TEXT NOT NULL)",
			"INSERT INTO tickets(id, label) VALUES(" + maxRowid + ", 'max')",
			"INSERT INTO tickets(label) VALUES('next')",
		},
	})
	fmt.Printf("5. an insert without an id after rowid %s (SQLite picks a random unused rowid): %s\n", maxRowid, rowidDiff)
	check(!rowidDiff.Equal, "rowid assignment past the maximum rowid makes two runs differ")

	fmt.Println("\n== a source of variation the outline did not list: connection sessions ==")
	walSessions := sessionVariant(ctx, r, dir)
	deleteSessions := sessionVariant(ctx, deleteRecipe, dir)
	walSessionsNoPage1 := sessionsWithoutPage1(ctx, r, dir)
	fmt.Printf("6. WAL mode, steps 0-%d one connection per step vs one connection for all steps: %s\n", variantBase, walSessions)
	fmt.Printf("   rollback mode, the same comparison: %s\n", deleteSessions)
	check(onlyChangeCounter(walSessions), "FINDING: in WAL mode the number of connection sessions changes the file, and only the change counter (bytes 24-27 and 92-95)")
	check(deleteSessions.Equal, "in rollback mode the number of connection sessions does not change the file")
	fmt.Printf("   WAL mode, recipe to step %d, then %d commits that never write page 1, one connection per commit vs one connection for all: %s\n", variantBase, page1UntouchedCommits, walSessionsNoPage1)
	check(walSessionsNoPage1.Equal, "in WAL mode only a commit that writes page 1 writes the change counter: sessions whose commits never touch page 1 leave the file identical")

	fmt.Println("\n== a source of variation the outline did not list: rolled-back work inside a committed transaction ==")
	walAborted, walAbortedUnused := abortedSavepointVariant(ctx, r, dir)
	deleteAborted, deleteAbortedUnused := abortedSavepointVariant(ctx, deleteRecipe, dir)
	fmt.Printf("7. recipe to step %d, then one transaction committing one INSERT; run 2 first inserts %d documents of %d bytes inside a SAVEPOINT and rolls back to it\n", variantBase, abortedDocuments, abortedDocumentBytes)
	fmt.Printf("   WAL mode, secure_delete=ON: %s\n", walAborted)
	for _, kind := range walAbortedUnused.Kinds {
		fmt.Printf("     %s\n", kind)
	}
	fmt.Printf("   rollback mode, secure_delete=ON: %s\n", deleteAborted)
	for _, kind := range deleteAbortedUnused.Kinds {
		fmt.Printf("     %s\n", kind)
	}
	walRest := slices.DeleteFunc(slices.Clone(walAborted.DifferingPages), func(page int) bool { return page == 1 })
	check(!walAborted.Equal && onlyCounterBytes(walAborted.Page1Bytes) && len(walRest) > 0 && slices.Equal(walRest, walAbortedUnused.LeafPages), "WAL mode: a rolled-back SAVEPOINT changes the committed file even with secure_delete=ON: reused freelist leaf pages keep the rolled-back bytes, and page 1's change counter moves")
	check(!deleteAborted.Equal && deleteAbortedUnused.OnlyUnused && slices.Equal(deleteAbortedUnused.LeafPages, deleteAborted.DifferingPages), "rollback mode: a rolled-back SAVEPOINT leaves the rolled-back bytes in reused freelist leaf pages (secure_delete=ON does not clear them)")

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("RECIPE REBUILD FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("RECIPE REBUILD OK")
}

const touchAccountSQL = "UPDATE accounts SET balance = balance + 1 WHERE id = 1"

func secureDeleteFastVariant(ctx context.Context, r recipe.Recipe, dir string) (fileDiff, unusedReport) {
	batchVariant(ctx, r, dir, "FAST")
	on := filepath.Join(dir, "batches-"+r.JournalMode+"-secure-delete-on-one-per-step.db")
	fast := filepath.Join(dir, "batches-"+r.JournalMode+"-secure-delete-fast-one-per-step.db")
	d := compareFiles(on, fast)
	return d, classifyDifferences(on, fast, d)
}

func sessionsWithoutPage1(ctx context.Context, r recipe.Recipe, dir string) fileDiff {
	perSession := filepath.Join(dir, "page1-untouched-"+r.JournalMode+"-one-per-commit.db")
	oneSession := filepath.Join(dir, "page1-untouched-"+r.JournalMode+"-one-for-all.db")
	for _, path := range []string{perSession, oneSession} {
		buildBase(ctx, r, path)
	}
	for range page1UntouchedCommits {
		applyStatementsInSession(ctx, r, perSession, nil, touchAccountSQL)
	}
	db, err := r.Open(ctx, oneSession)
	if err != nil {
		fatal("open", err)
	}
	for range page1UntouchedCommits {
		if _, err := db.ExecContext(ctx, touchAccountSQL); err != nil {
			fatal("update", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	if err := recipe.CheckAtRest(oneSession); err != nil {
		fatal("at rest", err)
	}
	fmt.Printf("INFO: page1-untouched sha256 one-per-commit %s, one-for-all %s\n", hashOf(perSession).Exact, hashOf(oneSession).Exact)
	return compareFiles(perSession, oneSession)
}

const (
	page1UntouchedCommits = 3
	abortedDocuments      = 10
	abortedDocumentBytes  = 12000
	committedEventSQL     = "INSERT INTO events(id, account_id, kind, amount, note) VALUES(900003, 1, 'fee', 42, 'committed')"
)

func abortedSavepointVariant(ctx context.Context, r recipe.Recipe, dir string) (fileDiff, unusedReport) {
	clean := filepath.Join(dir, "savepoint-"+r.JournalMode+"-never-run.db")
	aborted := filepath.Join(dir, "savepoint-"+r.JournalMode+"-rolled-back.db")
	for _, path := range []string{clean, aborted} {
		buildBase(ctx, r, path)
	}
	applyStatementsInSession(ctx, r, clean, nil, committedEventSQL)
	insertDocuments := fmt.Sprintf("WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < %d) INSERT INTO documents(id, account_id, title, body) SELECT 900000 + i, 1, 'aborted', unhex(replace(hex(zeroblob(%d)), '00', 'AB')) FROM n", abortedDocuments-1, abortedDocumentBytes)
	applyStatementsInSession(ctx, r, aborted, nil, "SAVEPOINT aborted", insertDocuments, "ROLLBACK TO aborted", "RELEASE aborted", committedEventSQL)
	fmt.Printf("INFO: savepoint %s sha256 never-run %s, rolled-back %s\n", r.JournalMode, hashOf(clean).Exact, hashOf(aborted).Exact)
	d := compareFiles(clean, aborted)
	return d, classifyDifferences(clean, aborted, d)
}

func runTwiceWithPlanChange(ctx context.Context, r recipe.Recipe, dir, name string, statements ...string) fileDiff {
	a := filepath.Join(dir, name+"-run1.db")
	b := filepath.Join(dir, name+"-run2.db")
	buildBase(ctx, r, a)
	applyStatementsInSession(ctx, r, a, nil, statements...)
	buildBase(ctx, r, b)
	applyStatementsInSession(ctx, r, b, []string{"PRAGMA reverse_unordered_selects=ON"}, statements...)
	return compareFiles(a, b)
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
