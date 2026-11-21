// Experiment 2.3 demonstrates Rebuild Ladder rung R1: capturing every page of
// the closed R0 database with internal/capture into pages.bin plus a per-page
// SHA-256 manifest, rebuilding a byte-identical (E1) database from the capture
// alone, refusing five kinds of tampered captures with the offending page named,
// and refusing databases that are not at rest (uncheckpointed -wal or hot -journal).
//
// Run from the repo root:
//
//	go run ./ch02-file-format-and-pages/experiments/2.3-page-manifest
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"

	_ "modernc.org/sqlite"
)

const (
	childLeaveWAL         = "child-leave-wal"
	childLeaveHotJournal  = "child-leave-hot-journal"
	childSpillCacheSize   = 10
	childExitWithoutClose = 3
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

func must[T any](value T, err error) func(string) T {
	return func(what string) T {
		if err != nil {
			fatal(what, err)
		}
		return value
	}
}

func relative(dir, text string) string {
	return strings.ReplaceAll(text, dir+string(os.PathSeparator), "")
}

func integrityCheck(ctx context.Context, path string) string {
	db := must(recipe.Default().Open(ctx, path))("open " + filepath.Base(path))
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		fatal("integrity_check", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return result
}

func roleSummary(m equal.Manifest) string {
	counts := map[string]int{}
	for _, e := range m.Entries {
		counts[e.Role]++
	}
	var parts []string
	for _, r := range dbfile.AllRoles {
		if counts[r.String()] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", r, counts[r.String()]))
		}
	}
	return strings.Join(parts, " ")
}

func partA(ctx context.Context, dir string) (string, string) {
	fmt.Println()
	fmt.Println("== A. capture: every page of the closed R0 database, read by internal/dbfile, plus a manifest (page, role, sha256)")
	r := recipe.Default()
	var manifests []equal.Manifest
	var paths, captures []string
	for run := 1; run <= 2; run++ {
		path := filepath.Join(dir, fmt.Sprintf("run-%d", run), "r0.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal("mkdir", err)
		}
		if err := r.Build(ctx, path, r.Steps); err != nil {
			fatal("build", err)
		}
		captureDir := filepath.Join(dir, fmt.Sprintf("run-%d", run), "capture")
		m := must(capture.Pages(path, captureDir))("capture")
		fmt.Printf("run %d: %s built to step %d -> %s: %d pages, manifest sha256 %s\n", run, relative(dir, path), r.Steps, relative(dir, captureDir), m.PageCount(), m.SHA256())
		manifests = append(manifests, m)
		paths = append(paths, path)
		captures = append(captures, captureDir)
	}
	fmt.Printf("roles in the manifest: %s\n", roleSummary(manifests[0]))
	fmt.Printf("capture files: %s (%d bytes = %d pages x %d) and %s\n", capture.PagesFile, must(os.Stat(filepath.Join(captures[0], capture.PagesFile)))("stat").Size(), manifests[0].PageCount(), manifests[0].PageSize, capture.ManifestFile)
	fmt.Println("manifest of run 1:")
	for _, line := range strings.Split(strings.TrimSuffix(manifests[0].Text(), "\n"), "\n") {
		fmt.Printf("  %s\n", line)
	}
	diff := equal.CompareManifests(manifests[0], manifests[1])
	fmt.Printf("run 1 vs run 2: %s\n", diff)
	check(diff.Equal && manifests[0].Text() == manifests[1].Text(), "two runs of the recipe give manifests identical line for line")
	return paths[0], captures[0]
}

func partB(ctx context.Context, dir, original, captureDir string) {
	fmt.Println()
	fmt.Println("== B. rebuild from the capture alone and prove it equal (E1)")
	rebuilt := filepath.Join(dir, "rebuilt", "from-capture.db")
	if err := os.MkdirAll(filepath.Dir(rebuilt), 0o755); err != nil {
		fatal("mkdir", err)
	}
	m := must(capture.Rebuild(captureDir, rebuilt))("rebuild")
	fmt.Printf("rebuilt %s from %s: %d pages written in order\n", relative(dir, rebuilt), relative(dir, captureDir), m.PageCount())
	result := must(equal.CompareFiles(original, rebuilt, m.PageSize))("compare")
	fmt.Printf("original vs rebuilt: %s\n", result)
	check(result.Equal, "sha256 of the rebuild equals sha256 of the original")
	match := must(recipe.Default().MatchStep(ctx, rebuilt))("match step")
	fmt.Printf("recipe lookup of the rebuild: %s\n", match)
	check(match.Found() && match.Exact && match.Step == 30, "the rebuild is the recipe's state after step 30, byte for byte")
	integrity := integrityCheck(ctx, rebuilt)
	fmt.Printf("integrity_check of the rebuild: %s\n", integrity)
	check(integrity == "ok", "SQLite opens the rebuild and integrity_check returns ok")
	again := must(equal.FileSHA256(rebuilt))("hash")
	check(again == result.SHA256A && recipe.CheckAtRest(rebuilt) == nil, "opening the rebuild with SQLite and closing it changed nothing and left no side files")

	fmt.Println("one more recipe state for comparison: the manifest of the database after step 29")
	step29 := filepath.Join(dir, "step-29", "r.db")
	if err := os.MkdirAll(filepath.Dir(step29), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.Default().Build(ctx, step29, 29); err != nil {
		fatal("build step 29", err)
	}
	m29 := must(capture.Pages(step29, filepath.Join(dir, "step-29", "capture")))("capture step 29")
	diff := equal.CompareManifests(m29, m)
	fmt.Printf("step 29 vs step 30: %s\n", diff)
	check(!diff.Equal && diff.FirstDifferingPage == 1, "the manifest comparison names the first differing page (page 1: the header changes on every recipe step)")
}

// tamper defines one capture-corruption test case and the page-naming substring
// that capture.Rebuild's refusal must contain.
type tamper struct {
	name string
	want string
	edit func(entries []string) []string
	page func(pages []byte, pageSize int)
}

func partC(dir, captureDir string) {
	fmt.Println()
	fmt.Println("== C. a damaged capture is refused, and the refusal names the page")
	original := must(os.ReadFile(filepath.Join(captureDir, capture.ManifestFile)))("read manifest")
	pages := must(os.ReadFile(filepath.Join(captureDir, capture.PagesFile)))("read pages")
	cases := []tamper{
		{name: "drop the manifest entry for page 100", want: "page 100: missing", edit: func(e []string) []string { return append(e[:99:99], e[100:]...) }},
		{name: "list page 50 twice (in place of page 51)", want: "page 50: listed twice", edit: func(e []string) []string { e[50] = e[49]; return e }},
		{name: "swap the entries for pages 120 and 121", want: "page 121: out of order", edit: func(e []string) []string { e[119], e[120] = e[120], e[119]; return e }},
		{name: "flip one byte in the image of page 200", want: "capture page 200:", page: func(p []byte, size int) { p[199*size+1000] ^= 0x01 }},
		{name: "swap the images of pages 7 and 8", want: "capture page 7:", page: func(p []byte, size int) {
			a := append([]byte(nil), p[6*size:7*size]...)
			copy(p[6*size:7*size], p[7*size:8*size])
			copy(p[7*size:8*size], a)
		}},
	}
	for i, c := range cases {
		target := filepath.Join(dir, "tampered", fmt.Sprintf("c%d", i+1))
		if err := os.MkdirAll(target, 0o755); err != nil {
			fatal("mkdir", err)
		}
		manifestText := string(original)
		pageBytes := append([]byte(nil), pages...)
		if c.edit != nil {
			lines := strings.Split(strings.TrimSuffix(manifestText, "\n"), "\n")
			entries := c.edit(append([]string(nil), lines[3:]...))
			lines = append(lines[:3:3], entries...)
			lines[2] = fmt.Sprintf("pages %d", len(entries))
			manifestText = strings.Join(lines, "\n") + "\n"
			if len(entries)*4096 < len(pageBytes) {
				pageBytes = pageBytes[:len(entries)*4096]
			}
		}
		if c.page != nil {
			c.page(pageBytes, 4096)
		}
		if err := os.WriteFile(filepath.Join(target, capture.ManifestFile), []byte(manifestText), 0o644); err != nil {
			fatal("write", err)
		}
		if err := os.WriteFile(filepath.Join(target, capture.PagesFile), pageBytes, 0o644); err != nil {
			fatal("write", err)
		}
		out := filepath.Join(target, "rebuilt.db")
		_, err := capture.Rebuild(target, out)
		_, statErr := os.Stat(out)
		fmt.Printf("  C%d %s\n    -> %v\n", i+1, c.name, err)
		check(err != nil && strings.Contains(err.Error(), c.want) && errors.Is(statErr, os.ErrNotExist), fmt.Sprintf("C%d: the rebuild is refused naming %q, and no output file is written", i+1, c.want))
	}
}

func runChild(mode, path string) int {
	command := must(trace.SelfCommand(mode, path))("self command")
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		fatal("child", err)
	}
	return 0
}

// childLeave runs in a separate process that leaves the database not at rest
// and exits with status 3 without closing its SQLite handle. In WAL mode it
// commits one UPDATE so frames remain in -wal; in rollback mode it sets a tiny
// cache_size=10 so updating every document spills dirty pages to the database
// file and syncs a hot -journal header (src/pager.c:1494-1501, 4377) before exit.
func childLeave(mode, path string) {
	ctx := context.Background()
	r := recipe.Default()
	options := r.SQLiteOptions()
	if mode == childLeaveHotJournal {
		options = r.WithJournalMode("delete").SQLiteOptions()
		options.Conn.CacheSize = childSpillCacheSize
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if mode == childLeaveWAL {
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1000 WHERE id = 1"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(childExitWithoutClose)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE documents SET body = zeroblob(length(body))")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(childExitWithoutClose)
}

func copyMainFileOnly(from, to string) {
	if err := os.WriteFile(to, must(os.ReadFile(from))("read"), 0o644); err != nil {
		fatal("copy", err)
	}
}

// queryString runs a single-column SQL query against path (passing mode=ro via
// url.Values when readOnly is true) and fails immediately on query or close error.
func queryString(ctx context.Context, path string, readOnly bool, query string) string {
	values := url.Values{}
	if readOnly {
		values.Set("mode", "ro")
	}
	uri := must(sqlitex.FileURI(path, values))("uri")
	db := must(sql.Open("sqlite", uri))("open")
	db.SetMaxOpenConns(1)
	var value string
	if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
		fatal(query, err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	return value
}

func sideFiles(path string) string {
	var parts []string
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if info, err := os.Stat(path + suffix); err == nil {
			parts = append(parts, fmt.Sprintf("%s %d bytes", suffix, info.Size()))
		}
	}
	if len(parts) == 0 {
		return "no side files"
	}
	return strings.Join(parts, ", ")
}

func partD(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== D. a database that is not at rest is refused, because its file alone is stale or torn")
	r := recipe.Default()

	fmt.Println("D1. WAL mode: a child process commits one UPDATE and exits without closing its connection")
	walDir := filepath.Join(dir, "not-at-rest", "wal")
	if err := os.MkdirAll(walDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(walDir, "r.db")
	if err := r.Build(ctx, path, 6); err != nil {
		fatal("build", err)
	}
	before := must(equal.FileSHA256(path))("hash")
	exit := runChild(childLeaveWAL, path)
	fmt.Printf("  child exit status %d; %s\n", exit, sideFiles(path))
	_, err := capture.Pages(path, filepath.Join(walDir, "capture"))
	fmt.Printf("  capture -> %s\n", relative(walDir, fmt.Sprint(err)))
	check(errors.Is(err, capture.ErrNotAtRest), "D1: the capture is refused: the -wal holds committed frames")
	stale := filepath.Join(walDir, "stale-copy.db")
	copyMainFileOnly(path, stale)
	balanceQuery := "SELECT balance FROM accounts WHERE id = 1"
	staleBalance := queryString(ctx, stale, true, balanceQuery)
	fmt.Printf("  the database file alone: sha256 unchanged by the commit: %v; account 1 balance %s\n", must(equal.FileSHA256(path))("hash") == before, staleBalance)
	liveBalance := queryString(ctx, path, false, balanceQuery)
	fmt.Printf("  SQLite opening the database with its -wal: account 1 balance %s; after the last close: %s\n", liveBalance, sideFiles(path))
	check(staleBalance != liveBalance, "D1: a page capture taken from the file alone would have lost the committed UPDATE")
	m := must(capture.Pages(path, filepath.Join(walDir, "capture")))("capture after checkpoint")
	fmt.Printf("  capture after SQLite checkpointed and closed: %d pages, manifest sha256 %s\n", m.PageCount(), m.SHA256())
	check(must(equal.FileSHA256(path))("hash") != before, "D1: once at rest, the capture includes the UPDATE (the file changed at the checkpoint)")

	fmt.Printf("D2. rollback mode: a child process with cache_size=%d updates every document body in one transaction, spills dirty pages to the file, and exits before COMMIT\n", childSpillCacheSize)
	journalDir := filepath.Join(dir, "not-at-rest", "journal")
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	path = filepath.Join(journalDir, "r.db")
	rollback := r.WithJournalMode("delete")
	if err := rollback.Build(ctx, path, 6); err != nil {
		fatal("build", err)
	}
	before = must(equal.FileSHA256(path))("hash")
	bodyHashQuery := "SELECT count(*) || ' rows, ' || sum(body = zeroblob(length(body))) || ' bodies all zero' FROM documents"
	exit = runChild(childLeaveHotJournal, path)
	fmt.Printf("  child exit status %d; %s\n", exit, sideFiles(path))
	_, err = capture.Pages(path, filepath.Join(journalDir, "capture"))
	fmt.Printf("  capture -> %s\n", relative(journalDir, fmt.Sprint(err)))
	check(errors.Is(err, capture.ErrNotAtRest), "D2: the capture is refused: a hot -journal exists")
	torn := filepath.Join(journalDir, "torn-copy.db")
	copyMainFileOnly(path, torn)
	changed := must(equal.FileSHA256(path))("hash") != before
	tornView := queryString(ctx, torn, true, bodyHashQuery)
	fmt.Printf("  the database file alone: changed by the spill: %v; documents: %s\n", changed, tornView)
	recovered := queryString(ctx, path, false, bodyHashQuery)
	fmt.Printf("  SQLite opening the database with its hot journal (rolls it back): documents: %s; after the last close: %s\n", recovered, sideFiles(path))
	check(changed && tornView != recovered, "D2: the file alone holds part of an uncommitted transaction; a capture of it would be torn")
	check(must(equal.FileSHA256(path))("hash") == before, "D2: after SQLite rolled the hot journal back, the file is byte-identical to its state before the transaction")
	m = must(capture.Pages(path, filepath.Join(journalDir, "capture")))("capture after rollback")
	fmt.Printf("  capture after the rollback: %d pages\n", m.PageCount())
}

func main() {
	if len(os.Args) == 3 && (os.Args[1] == childLeaveWAL || os.Args[1] == childLeaveHotJournal) {
		childLeave(os.Args[1], os.Args[2])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("2.3")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	original, captureDir := partA(ctx, dir)
	partB(ctx, dir, original, captureDir)
	partC(dir, captureDir)
	partD(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("MANIFEST FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("MANIFEST OK")
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
