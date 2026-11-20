// Experiment 5.1 (wal-inspector) parses a live SQLite WAL and its -shm
// wal-index in plain Go, verifies the running checksum chain and commit frames
// against PRAGMA wal_checkpoint(NOOP) and SQLite's showwal utility, and proves
// that repeated runs and different synchronous settings produce the exact same
// frame sequence, differing only in random salts and dependent checksums.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"

	_ "modernc.org/sqlite"
)

const (
	firstStep = 1
	lastStep  = 5
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

type run struct {
	name        string
	synchronous string
	wal         *walfile.File
	walBytes    []byte
	index       *walfile.Index
	noop        sqlitex.CheckpointResult
	pageCounts  []int
	databaseDir string
}

// buildRun creates a step-0 database, applies recipe steps 1..5 on one
// connection with autocheckpoint disabled, copies the live database, -wal, and
// -shm from a child process before closing, and parses both sidecar files.
func buildRun(ctx context.Context, dir, name, synchronous string) run {
	r := recipe.Default()
	runDir := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(runDir, "copy"), 0o755); err != nil {
		fatal("mkdir", err)
	}
	path := filepath.Join(runDir, "r.db")
	if err := r.Build(ctx, path, firstStep-1); err != nil {
		fatal("build step 0", err)
	}
	options := r.SQLiteOptions()
	// Disable autocheckpoint so every frame of steps 1..5 remains in the WAL
	// until the child copies it.
	options.Conn.WALAutocheckpoint = 0
	options.Conn.Synchronous = synchronous
	db := must(sqlitex.Open(ctx, path, options))("open")
	result := run{name: name, synchronous: synchronous, databaseDir: runDir}
	for k := firstStep; k <= lastStep; k++ {
		if err := r.ApplyStep(ctx, db, k); err != nil {
			fatal(fmt.Sprintf("step %d", k), err)
		}
		var pages int
		if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
			fatal("page_count", err)
		}
		result.pageCounts = append(result.pageCounts, pages)
	}
	// Copy while the connection is still open because closing the last
	// connection triggers a close checkpoint and deletes -wal and -shm
	// (src/wal.c:2528-2565, sqlite3WalClose). Copying in a child avoids
	// dropping this process's POSIX locks on -shm (src/os_unix.c:4703-4750).
	copies := must(walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(path, filepath.Join(runDir, "copy"), "r.db")))("live copy")
	for _, c := range copies {
		if !c.Present {
			fatal("live copy", fmt.Errorf("%s was missing while the connection was open", filepath.Base(c.From)))
		}
	}
	// NOOP takes no locks and reports (busy, mxFrame, nBackfill) from the
	// wal-index (src/wal.c:4341, 4399-4403).
	result.noop = must(sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointNoop))("NOOP checkpoint")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	result.walBytes = must(os.ReadFile(filepath.Join(runDir, "copy", "r.db-wal")))("read WAL copy")
	result.wal = must(walfile.Parse(result.walBytes))("parse WAL")
	result.index = must(walfile.ParseIndexFile(filepath.Join(runDir, "copy", "r.db-shm")))("parse wal-index")
	return result
}

func shortHash(h string) string { return h[:16] }

func partA(ctx context.Context, dir string) run {
	fmt.Println()
	fmt.Printf("== A. one WAL holding recipe steps %d-%d: one connection, wal_autocheckpoint=0, synchronous=FULL\n", firstStep, lastStep)
	first := buildRun(ctx, dir, "run-1", sqlitex.SyncFull)
	wal := first.wal
	fmt.Printf("base: recipe step %d built and closed; then steps %d-%d committed on one connection; a child process copied the database, -wal and -shm while that connection was still open\n", firstStep-1, firstStep, lastStep)
	fmt.Printf("WAL header: magic=0x%08x (%s checksums) version=%d page-size=%d checkpoint-seq=%d\n",
		wal.Header.Magic, wal.Header.ChecksumByteOrder(), wal.Header.Version, wal.Header.PageSize, wal.Header.CheckpointSequence)
	fmt.Printf("INFO: salt-1=0x%08x salt-2=0x%08x header checksum %s\n", wal.Header.Salt1, wal.Header.Salt2, wal.Header.Checksum)
	fmt.Printf("WAL size %d bytes = %d + %d frames x (%d + %d)\n", wal.Size, walfile.HeaderSize, len(wal.Frames), walfile.FrameHeaderSize, wal.PageSize())
	check(wal.Size == walfile.HeaderSize+int64(len(wal.Frames))*int64(walfile.FrameHeaderSize+wal.PageSize()) && wal.PartialFrameBytes == 0,
		"the WAL is a 32-byte header plus whole frames of a 24-byte header and one 4096-byte page")
	fmt.Println("frames (number, page, commit size, sha256 of the page image, first 16 hex digits):")
	for _, frame := range wal.Frames {
		marker := ""
		if frame.IsCommit() {
			marker = "  <- commit"
		}
		fmt.Printf("  frame %3d  page %3d  commit-size %3d  sha256 %s%s\n", frame.Number, frame.Page, frame.CommitSize, shortHash(frame.PageSHA256), marker)
	}
	saltsMatch := true
	for _, frame := range wal.Frames {
		saltsMatch = saltsMatch && frame.SaltsMatchHeader
	}
	fmt.Printf("checksum chain: %d of %d frames verify, stop reason: %s\n", wal.ValidFrames, len(wal.Frames), wal.StopReason())
	check(wal.ValidFrames == len(wal.Frames) && saltsMatch, "every frame carries the header's salts and continues the running checksum chain")
	commits := wal.Commits()
	fmt.Println("transactions (a commit frame is the only frame with a non-zero commit size):")
	sizesMatch := len(commits) == len(first.pageCounts)
	for i, c := range commits {
		pageCount := -1
		if i < len(first.pageCounts) {
			pageCount = first.pageCounts[i]
		}
		fmt.Printf("  commit %d = recipe step %d: frames %d-%d (%d frames), commit size %d pages; PRAGMA page_count after the step: %d\n",
			i+1, firstStep+i, c.FirstFrame, c.LastFrame, c.Frames(), c.Pages, pageCount)
		sizesMatch = sizesMatch && int(c.Pages) == pageCount
	}
	check(len(commits) == lastStep-firstStep+1, "five recipe steps left exactly five commit frames")
	check(sizesMatch, "each commit frame's size field equals the database's page count after that transaction")
	fmt.Printf("PRAGMA wal_checkpoint(NOOP) on the open connection: %s\n", first.noop)
	check(first.noop.LogFrames == len(wal.Frames) && first.noop.Checkpointed == 0, "the frame count equals the log column of wal_checkpoint(NOOP); nothing was checkpointed")
	describeIndex(first)
	crossCheckShowwal(first)
	return first
}

func describeIndex(r run) {
	h := r.index.Header()
	wal := r.wal
	fmt.Printf("wal-index (-shm copy): %d bytes, %s, header copies agree: %v, header checksum ok: %v\n", r.index.Size, r.index.ByteOrderName(), r.index.HeadersAgree(), h.ChecksumOK)
	fmt.Printf("  mxFrame=%d nPage=%d page-size=%d nBackfill=%d salts equal the WAL header: %v\n", h.MaxFrame, h.Pages, h.PageSize(), r.index.Checkpoint.Backfill, h.Salt1 == wal.Header.Salt1 && h.Salt2 == wal.Header.Salt2)
	pages, err := r.index.FramePages(h.MaxFrame)
	mappingMatches := err == nil && len(pages) == len(wal.Frames)
	for i := 0; mappingMatches && i < len(pages); i++ {
		mappingMatches = pages[i] == wal.Frames[i].Page
	}
	fmt.Printf("  page-number array for frames 1-%d equals the WAL's frame pages: %v\n", h.MaxFrame, mappingMatches)
	check(r.index.HeadersAgree() && h.ChecksumOK && int(h.MaxFrame) == wal.LastCommitFrame && h.Pages == wal.DatabasePages() &&
		h.Salt1 == wal.Header.Salt1 && h.Salt2 == wal.Header.Salt2 && mappingMatches,
		"the -shm header names the last commit frame (mxFrame), the database size and the WAL's salts, and its page array maps every frame to its page")
}

func crossCheckShowwal(r run) {
	tool, err := sqlitex.LocateTool("showwal")
	if err != nil {
		fmt.Printf("NO-SHOWWAL: %v; cross-check skipped\n", err)
		return
	}
	out, err := exec.Command(tool, filepath.Join(r.databaseDir, "copy", "r.db-wal")).Output()
	if err != nil {
		fatal("showwal", err)
	}
	listing := must(walfile.ParseShowwal(string(out)))("parse showwal output")
	problems := r.wal.CompareWithShowwal(listing)
	fmt.Printf("showwal (built from the pinned tree) lists %d frames; disagreements with walfile: %d\n", len(listing.Frames), len(problems))
	for _, p := range problems {
		fmt.Printf("  %s\n", p)
	}
	check(len(problems) == 0, "showwal and walfile agree on every frame's page, commit size, salts and stored checksums, and on which checksums verify")
}

func partB(ctx context.Context, dir string, first run) {
	fmt.Println()
	fmt.Println("== B. the same workload again: what differs between two WALs")
	second := buildRun(ctx, dir, "run-2", sqlitex.SyncFull)
	fmt.Printf("INFO: run 2 salt-1=0x%08x salt-2=0x%08x\n", second.wal.Header.Salt1, second.wal.Header.Salt2)
	comparison := walfile.CompareBytes(first.walBytes, second.walBytes, first.wal.PageSize())
	fmt.Printf("byte comparison run 1 vs run 2: sizes %d and %d\n", comparison.SizeA, comparison.SizeB)
	for r := walfile.RegionHeaderMagic; r <= walfile.RegionBeyondShorterFile; r++ {
		changed := comparison.ByRegion[r] > 0
		fmt.Printf("  %-40s %s\n", r.String()+":", map[bool]string{true: "differs", false: "same"}[changed])
	}
	fmt.Printf("INFO: differing bytes per region: %s\n", comparison)
	check(!comparison.Identical() && comparison.OnlyIn(walfile.RegionHeaderSalt, walfile.RegionHeaderChecksum, walfile.RegionFrameSalt, walfile.RegionFrameChecksum),
		"the two WALs differ, and only inside the salt and checksum fields")
	equal, at := walfile.EqualTuples(first.wal.Tuples(true), second.wal.Tuples(true))
	fmt.Printf("frame sequences as (page, page sha256, commit size): equal %v (first difference at frame %d, 0 = none)\n", equal, at)
	check(equal, "the two runs wrote the same pages, with the same images, in the same order, with the same commit sizes")

	fmt.Println()
	fmt.Println("== C. other synchronous levels write the same frames")
	for _, level := range []string{sqlitex.SyncNormal, sqlitex.SyncOff} {
		other := buildRun(ctx, dir, "sync-"+strings.ToLower(level), level)
		equal, at := walfile.EqualTuples(first.wal.Tuples(true), other.wal.Tuples(true))
		fmt.Printf("synchronous=%s: %d frames, %d commits; frame sequence equal to synchronous=FULL: %v (first difference at frame %d)\n",
			level, len(other.wal.Frames), len(other.wal.Commits()), equal, at)
		check(equal, fmt.Sprintf("synchronous=%s changes when SQLite syncs, not which frames it writes", level))
	}
}

func main() {
	// Re-exec entry point for child-process live copies.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	dir := sqlitex.MustWorkDir("5.1")
	fmt.Printf("INFO: work dir %s\n", displayPath(dir))
	first := partA(ctx, dir)
	partB(ctx, dir, first)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("WAL INSPECTOR FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("WAL INSPECTOR OK")
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
