// wal-inspector-edge attacks Experiment 5.1 and internal/walfile's parser,
// checksum chain, byte-diff, showwal cross-check, -shm decoder, and pin checks.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/walfile"

	_ "modernc.org/sqlite"
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

// buildSyntheticWAL constructs a well-formed WAL byte slice with the requested
// magic (0x377f0682 little-endian or 0x377f0683 big-endian), version, page size,
// checkpoint sequence, salts, and frame definitions.
type synthFrame struct {
	page       uint32
	commitSize uint32
	data       []byte
}

func buildSyntheticWAL(magic, version, pageSize, ckptSeq, salt1, salt2 uint32, frames []synthFrame) ([]byte, error) {
	bigEnd := magic&1 == 1
	hdr := make([]byte, walfile.HeaderSize)
	be := binary.BigEndian
	be.PutUint32(hdr[0:], magic)
	be.PutUint32(hdr[4:], version)
	be.PutUint32(hdr[8:], pageSize)
	be.PutUint32(hdr[12:], ckptSeq)
	be.PutUint32(hdr[16:], salt1)
	be.PutUint32(hdr[20:], salt2)
	cksum, err := walfile.ChecksumBytes(bigEnd, hdr[:24], walfile.Checksum{})
	if err != nil {
		return nil, err
	}
	be.PutUint32(hdr[24:], cksum[0])
	be.PutUint32(hdr[28:], cksum[1])

	var out bytes.Buffer
	out.Write(hdr)
	chain := cksum
	for _, fr := range frames {
		page := fr.data
		if len(page) != int(pageSize) {
			page = make([]byte, pageSize)
			copy(page, fr.data)
		}
		fh := make([]byte, walfile.FrameHeaderSize)
		be.PutUint32(fh[0:], fr.page)
		be.PutUint32(fh[4:], fr.commitSize)
		be.PutUint32(fh[8:], salt1)
		be.PutUint32(fh[12:], salt2)
		chain, err = walfile.ChecksumBytes(bigEnd, fh[:8], chain)
		if err != nil {
			return nil, err
		}
		chain, err = walfile.ChecksumBytes(bigEnd, page, chain)
		if err != nil {
			return nil, err
		}
		be.PutUint32(fh[16:], chain[0])
		be.PutUint32(fh[20:], chain[1])
		out.Write(fh)
		out.Write(page)
	}
	return out.Bytes(), nil
}

// buildPageSizeWAL builds a real SQLite WAL database at pageSize, commits 3
// transactions with wal_autocheckpoint=0, live-copies db/-wal/-shm through the
// child helper, and returns parsed WAL/index plus NOOP checkpoint and per-commit
// page counts.
type pageSizeRun struct {
	walBytes   []byte
	wal        *walfile.File
	index      *walfile.Index
	noop       sqlitex.CheckpointResult
	pageCounts []int
	walCopy    string
}

func buildPageSizeWAL(ctx context.Context, dir string, pageSize int) (pageSizeRun, error) {
	if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
		return pageSizeRun{}, err
	}
	dbPath := filepath.Join(dir, "db.sqlite")
	opts := sqlitex.DefaultOptions()
	opts.File.PageSize = pageSize
	opts.File.JournalMode = sqlitex.JournalWAL
	opts.Conn.WALAutocheckpoint = 0
	opts.Conn.Synchronous = sqlitex.SyncFull

	// Establish schema and close cleanly so the WAL starts fresh on the next open.
	initDB, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return pageSizeRun{}, fmt.Errorf("init open: %w", err)
	}
	if _, err := initDB.ExecContext(ctx, "CREATE TABLE kv (id INTEGER PRIMARY KEY, val BLOB)"); err != nil {
		initDB.Close()
		return pageSizeRun{}, fmt.Errorf("create table: %w", err)
	}
	if err := initDB.Close(); err != nil {
		return pageSizeRun{}, fmt.Errorf("init close: %w", err)
	}

	db, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return pageSizeRun{}, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	var pageCounts []int
	payload := bytes.Repeat([]byte("A"), max(128, pageSize/2))
	for commit := 1; commit <= 3; commit++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return pageSizeRun{}, err
		}
		for j := 0; j < 6; j++ {
			id := commit*100 + j
			if _, err := tx.ExecContext(ctx, "INSERT INTO kv(id, val) VALUES (?, ?)", id, payload); err != nil {
				tx.Rollback()
				return pageSizeRun{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", commit)); err != nil {
			tx.Rollback()
			return pageSizeRun{}, err
		}
		if err := tx.Commit(); err != nil {
			return pageSizeRun{}, err
		}
		var pc int
		if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pc); err != nil {
			return pageSizeRun{}, err
		}
		pageCounts = append(pageCounts, pc)
	}

	copyDir := filepath.Join(dir, "copy")
	copies, err := walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(dbPath, copyDir, "db.sqlite"))
	if err != nil {
		return pageSizeRun{}, fmt.Errorf("live copy: %w", err)
	}
	for _, c := range copies {
		if !c.Present {
			return pageSizeRun{}, fmt.Errorf("missing %s", filepath.Base(c.From))
		}
	}
	noop, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointNoop)
	if err != nil {
		return pageSizeRun{}, fmt.Errorf("noop: %w", err)
	}
	walCopy := filepath.Join(copyDir, "db.sqlite-wal")
	walBytes, err := os.ReadFile(walCopy)
	if err != nil {
		return pageSizeRun{}, err
	}
	wf, err := walfile.Parse(walBytes)
	if err != nil {
		return pageSizeRun{}, fmt.Errorf("parse wal: %w", err)
	}
	idx, err := walfile.ParseIndexFile(filepath.Join(copyDir, "db.sqlite-shm"))
	if err != nil {
		return pageSizeRun{}, fmt.Errorf("parse shm: %w", err)
	}
	return pageSizeRun{
		walBytes:   walBytes,
		wal:        wf,
		index:      idx,
		noop:       noop,
		pageCounts: pageCounts,
		walCopy:    walCopy,
	}, nil
}

func runShowwal(tool, walPath string) (walfile.ShowwalListing, error) {
	out, err := exec.Command(tool, walPath).CombinedOutput()
	if err != nil {
		return walfile.ShowwalListing{}, fmt.Errorf("showwal %s: %w (%s)", walPath, err, strings.TrimSpace(string(out)))
	}
	return walfile.ParseShowwal(string(out))
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func copyTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func buildScratchRepo(repoRoot, scratchRepo string) error {
	if err := os.RemoveAll(scratchRepo); err != nil {
		return err
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if err := copyFile(filepath.Join(repoRoot, f), filepath.Join(scratchRepo, f)); err != nil {
			return err
		}
	}
	dirs := []string{
		"sqlite",
		"internal/sqlitex",
		"internal/recipe",
		"internal/dbfile",
		"internal/equal",
		"internal/walfile",
		"ch05-wal-transactions-checkpoints/experiments",
	}
	for _, d := range dirs {
		if err := copyTree(filepath.Join(repoRoot, d), filepath.Join(scratchRepo, d)); err != nil {
			return err
		}
	}
	return nil
}

type cmdResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runGoPkg(ctx context.Context, dir, pkg string, extraEnv []string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, "go", "run", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return cmdResult{}, err
		}
	}
	return cmdResult{exitCode: code, stdout: stdout.String(), stderr: stderr.String()}, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func main() {
	// Required so walfile.CopyLiveFiles / ReadLiveState can re-exec this binary
	// without touching database/-shm descriptors inside the process holding locks.
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch05", "wal-inspector-edge")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	showwalTool, showwalErr := sqlitex.LocateTool("showwal")

	// INS-01 (OUTLINE 5.1 bullet 1): All 8 valid SQLite page sizes (512..65536).
	{
		pageSizes := []int{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
		var problems []string
		for _, ps := range pageSizes {
			run1, err := buildPageSizeWAL(ctx, filepath.Join(scratch, fmt.Sprintf("ins01-%d-a", ps)), ps)
			if err != nil {
				problems = append(problems, fmt.Sprintf("ps=%d build: %v", ps, err))
				continue
			}
			wf := run1.wal
			wantSize := int64(walfile.HeaderSize) + int64(len(wf.Frames))*int64(walfile.FrameHeaderSize+ps)
			if wf.Size != wantSize || wf.PartialFrameBytes != 0 || wf.PageSize() != ps {
				problems = append(problems, fmt.Sprintf("ps=%d size/header mismatch (size=%d want=%d ps=%d)", ps, wf.Size, wantSize, wf.PageSize()))
			}
			if wf.ValidFrames != len(wf.Frames) || len(wf.Frames) == 0 {
				problems = append(problems, fmt.Sprintf("ps=%d valid=%d/%d", ps, wf.ValidFrames, len(wf.Frames)))
			}
			commits := wf.Commits()
			if len(commits) != len(run1.pageCounts) {
				problems = append(problems, fmt.Sprintf("ps=%d commits=%d want=%d", ps, len(commits), len(run1.pageCounts)))
			} else {
				for i, c := range commits {
					if int(c.Pages) != run1.pageCounts[i] {
						problems = append(problems, fmt.Sprintf("ps=%d commit %d size=%d want=%d", ps, i+1, c.Pages, run1.pageCounts[i]))
					}
				}
			}
			if run1.noop.LogFrames != len(wf.Frames) || run1.noop.Checkpointed != 0 {
				problems = append(problems, fmt.Sprintf("ps=%d noop=%s frames=%d", ps, run1.noop, len(wf.Frames)))
			}
			ih := run1.index.Header()
			if !run1.index.HeadersAgree() || !ih.ChecksumOK || ih.PageSize() != ps || int(ih.MaxFrame) != wf.LastCommitFrame || ih.Pages != wf.DatabasePages() {
				problems = append(problems, fmt.Sprintf("ps=%d shm mismatch (%s)", ps, ih))
			}
			idxPages, err := run1.index.FramePages(ih.MaxFrame)
			if err != nil || len(idxPages) != len(wf.Frames) {
				problems = append(problems, fmt.Sprintf("ps=%d shm FramePages err=%v len=%d/%d", ps, err, len(idxPages), len(wf.Frames)))
			} else {
				for i, pg := range idxPages {
					if pg != wf.Frames[i].Page {
						problems = append(problems, fmt.Sprintf("ps=%d shm frame %d page %d != %d", ps, i+1, pg, wf.Frames[i].Page))
						break
					}
				}
			}
			if showwalErr == nil {
				listing, err := runShowwal(showwalTool, run1.walCopy)
				if err != nil {
					problems = append(problems, fmt.Sprintf("ps=%d showwal: %v", ps, err))
				} else if diffs := wf.CompareWithShowwal(listing); len(diffs) > 0 {
					problems = append(problems, fmt.Sprintf("ps=%d showwal diffs=%v", ps, diffs[0]))
				}
			}
			// Repeat run at extremes (512 and 65536) to check salt/checksum-only diff.
			if ps == 512 || ps == 65536 {
				run2, err := buildPageSizeWAL(ctx, filepath.Join(scratch, fmt.Sprintf("ins01-%d-b", ps)), ps)
				if err != nil {
					problems = append(problems, fmt.Sprintf("ps=%d run2: %v", ps, err))
					continue
				}
				cmp := walfile.CompareBytes(run1.walBytes, run2.walBytes, ps)
				if cmp.Identical() || !cmp.OnlyIn(walfile.RegionHeaderSalt, walfile.RegionHeaderChecksum, walfile.RegionFrameSalt, walfile.RegionFrameChecksum) {
					problems = append(problems, fmt.Sprintf("ps=%d diff not salt/cksum only: %s", ps, cmp))
				}
				if eq, at := walfile.EqualTuples(wf.Tuples(true), run2.wal.Tuples(true)); !eq {
					problems = append(problems, fmt.Sprintf("ps=%d tuples differ at frame %d", ps, at))
				}
			}
		}
		report("INS-01",
			"real SQLite WAL + -shm + showwal + repeat-run diff across all 8 valid page sizes (512..65536)",
			"size formula, checksums, commit counts, NOOP, -shm PageSize/array, showwal, and salt/cksum-only diff hold at every page size",
			fmt.Sprintf("tested=8 page_sizes (showwal_checked=%v), problems=%v", showwalErr == nil, problems),
			len(problems) == 0)
	}

	// INS-02 (OUTLINE 5.1 bullet 2): WAL whose tail holds frames from an older generation.
	{
		dir := filepath.Join(scratch, "ins02")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir ins02", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(6)
		if err := r.Build(ctx, dbPath, 0); err != nil {
			fatal("build ins02", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open ins02", err)
		}
		for k := 1; k <= 5; k++ {
			if err := r.ApplyStep(ctx, db, k); err != nil {
				fatal("step ins02", err)
			}
		}
		gen1State, err := walfile.ReadLiveState(ctx, dbPath, dir)
		if err != nil {
			fatal("gen1 live state", err)
		}
		oldFrames := len(gen1State.WAL.Frames)
		oldSalt1, oldSalt2 := gen1State.WAL.Header.Salt1, gen1State.WAL.Header.Salt2

		// Complete PASSIVE checkpoint: backfills all frames without shrinking -wal.
		cpRes, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointPassive)
		if err != nil {
			fatal("passive ins02", err)
		}
		// Next commit restarts WAL at frame 1, leaving old-generation frames in the tail.
		if err := r.ApplyStep(ctx, db, 6); err != nil {
			fatal("step 6 ins02", err)
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(dbPath, copyDir, "r.db")); err != nil {
			fatal("live copy ins02", err)
		}
		noop, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointNoop)
		if err != nil {
			fatal("noop ins02", err)
		}
		// Let SQLite TRUNCATE the original for E1 replay verification.
		if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
			fatal("truncate ins02", err)
		}
		if err := db.Close(); err != nil {
			fatal("close ins02", err)
		}

		walCopy := filepath.Join(copyDir, "r.db-wal")
		wf, err := walfile.ParseFile(walCopy)
		if err != nil {
			fatal("parse ins02", err)
		}
		idx, err := walfile.ParseIndexFile(filepath.Join(copyDir, "r.db-shm"))
		if err != nil {
			fatal("parse shm ins02", err)
		}
		var problems []string
		if cpRes.Checkpointed != oldFrames {
			problems = append(problems, fmt.Sprintf("passive checkpointed %d != %d", cpRes.Checkpointed, oldFrames))
		}
		if len(wf.Frames) != oldFrames {
			problems = append(problems, fmt.Sprintf("total frames %d != old %d", len(wf.Frames), oldFrames))
		}
		if wf.ValidFrames == 0 || wf.ValidFrames >= oldFrames {
			problems = append(problems, fmt.Sprintf("expected partial valid frames, got %d/%d", wf.ValidFrames, oldFrames))
		}
		if wf.StopReason() != walfile.FrameSaltMismatch {
			problems = append(problems, fmt.Sprintf("stop reason=%v want FrameSaltMismatch", wf.StopReason()))
		}
		if int(idx.Header().MaxFrame) != wf.ValidFrames || noop.LogFrames != wf.ValidFrames {
			problems = append(problems, fmt.Sprintf("mxFrame=%d noop=%d valid=%d", idx.Header().MaxFrame, noop.LogFrames, wf.ValidFrames))
		}
		// Verify every tail frame carries the old generation's salts.
		tailOldSalts := 0
		for _, fr := range wf.Frames[wf.ValidFrames:] {
			if fr.Salt1 == oldSalt1 && fr.Salt2 == oldSalt2 {
				tailOldSalts++
			}
		}
		wantTail := oldFrames - wf.ValidFrames
		if tailOldSalts != wantTail {
			problems = append(problems, fmt.Sprintf("tail frames with old salts=%d want=%d", tailOldSalts, wantTail))
		}
		if showwalErr == nil {
			listing, err := runShowwal(showwalTool, walCopy)
			if err != nil {
				problems = append(problems, fmt.Sprintf("showwal: %v", err))
			} else if diffs := wf.CompareWithShowwal(listing); len(diffs) > 0 {
				problems = append(problems, fmt.Sprintf("showwal diffs=%v", diffs[0]))
			}
		}
		// Replay should ignore the older-generation tail and match SQLite's TRUNCATE E1.
		replayed := filepath.Join(copyDir, "replayed.db")
		rep, err := walfile.ReplayFiles(filepath.Join(copyDir, "r.db"), walCopy, replayed)
		if err != nil {
			problems = append(problems, fmt.Sprintf("replay err: %v", err))
		} else {
			if rep.IgnoredFrameCount() != wantTail {
				problems = append(problems, fmt.Sprintf("replay ignored=%d want=%d", rep.IgnoredFrameCount(), wantTail))
			}
			cmp, err := equal.CompareFiles(replayed, dbPath, 4096)
			if err != nil || !cmp.Equal {
				problems = append(problems, fmt.Sprintf("replay vs sqlite E1 equal=%v err=%v", cmp.Equal, err))
			}
		}
		report("INS-02",
			"WAL restarted after PASSIVE checkpoint leaving older-generation frames in tail",
			"walfile stops at FrameSaltMismatch, tail matches old salts, -shm & showwal agree, Replay matches SQLite E1",
			fmt.Sprintf("valid=%d/%d stop=%q tail_old_salts=%d/%d problems=%v", wf.ValidFrames, len(wf.Frames), wf.StopReason(), tailOldSalts, wantTail, problems),
			len(problems) == 0)
	}

	// INS-03 (OUTLINE 5.1 bullet 3): All 4 synchronous levels (OFF, NORMAL, FULL, EXTRA).
	{
		levels := []string{sqlitex.SyncOff, sqlitex.SyncNormal, sqlitex.SyncFull, sqlitex.SyncExtra}
		var baseWAL *walfile.File
		var baseBytes []byte
		var problems []string
		for _, lvl := range levels {
			dir := filepath.Join(scratch, "ins03-"+strings.ToLower(lvl))
			if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
				fatal("mkdir ins03", err)
			}
			dbPath := filepath.Join(dir, "r.db")
			r := recipe.Default()
			if err := r.Build(ctx, dbPath, 0); err != nil {
				fatal("build ins03", err)
			}
			opts := r.SQLiteOptions()
			opts.Conn.WALAutocheckpoint = 0
			opts.Conn.Synchronous = lvl
			db, err := sqlitex.Open(ctx, dbPath, opts)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s open: %v", lvl, err))
				continue
			}
			for k := 1; k <= 5; k++ {
				if err := r.ApplyStep(ctx, db, k); err != nil {
					db.Close()
					fatal("step ins03", err)
				}
			}
			copyDir := filepath.Join(dir, "copy")
			if _, err := walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(dbPath, copyDir, "r.db")); err != nil {
				db.Close()
				fatal("copy ins03", err)
			}
			db.Close()
			raw, err := os.ReadFile(filepath.Join(copyDir, "r.db-wal"))
			if err != nil {
				fatal("read ins03", err)
			}
			wf, err := walfile.Parse(raw)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s parse: %v", lvl, err))
				continue
			}
			if baseWAL == nil {
				baseWAL = wf
				baseBytes = raw
			} else {
				if eq, at := walfile.EqualTuples(baseWAL.Tuples(true), wf.Tuples(true)); !eq {
					problems = append(problems, fmt.Sprintf("%s tuples differ at frame %d", lvl, at))
				}
				cmp := walfile.CompareBytes(baseBytes, raw, 4096)
				if !cmp.OnlyIn(walfile.RegionHeaderSalt, walfile.RegionHeaderChecksum, walfile.RegionFrameSalt, walfile.RegionFrameChecksum) {
					problems = append(problems, fmt.Sprintf("%s diff outside salt/cksum: %s", lvl, cmp))
				}
			}
		}
		report("INS-03",
			"recipe steps 1-5 under all 4 PRAGMA synchronous levels (OFF, NORMAL, FULL, EXTRA)",
			"identical frame tuple sequences across all 4 levels; byte diffs confined solely to salts and checksums",
			fmt.Sprintf("frames=%d levels=4 problems=%v", len(baseWAL.Frames), problems),
			len(problems) == 0)
	}

	// INS-04: Synthetic WAL header/frame edge cases (big-endian magic vs showwal,
	// empty, header-only, ignored headers, bad version, page 0, trailing partial bytes).
	{
		var problems []string
		// (a) Big-endian checksum magic 0x377f0683 + showwal cross-check.
		beWAL, err := buildSyntheticWAL(walfile.MagicBigEndian, walfile.FormatVersion, 4096, 7, 0x11223344, 0x55667788, []synthFrame{
			{page: 1, commitSize: 0, data: bytes.Repeat([]byte{0xab}, 4096)},
			{page: 2, commitSize: 2, data: bytes.Repeat([]byte{0xcd}, 4096)},
		})
		if err != nil {
			fatal("synth big-endian", err)
		}
		beParsed, err := walfile.Parse(beWAL)
		if err != nil || beParsed.ValidFrames != 2 || !beParsed.Header.BigEndianChecksums() || beParsed.LastCommitFrame != 2 {
			problems = append(problems, fmt.Sprintf("big-endian parse failed: err=%v valid=%v", err, beParsed))
		} else if showwalErr == nil {
			bePath := filepath.Join(scratch, "big-endian.wal")
			if err := os.WriteFile(bePath, beWAL, 0o644); err != nil {
				fatal("write beWAL", err)
			}
			listing, err := runShowwal(showwalTool, bePath)
			if err != nil {
				problems = append(problems, fmt.Sprintf("showwal big-endian err: %v", err))
			} else if diffs := beParsed.CompareWithShowwal(listing); len(diffs) > 0 {
				problems = append(problems, fmt.Sprintf("showwal big-endian diffs: %v", diffs))
			}
		}

		// (b) Empty 0-byte WAL -> valid empty File.
		if f, err := walfile.Parse(nil); err != nil || len(f.Frames) != 0 || f.Size != 0 {
			problems = append(problems, fmt.Sprintf("empty wal: err=%v", err))
		}
		// (c) 32-byte header-only WAL -> valid with 0 frames.
		hdrOnly, _ := buildSyntheticWAL(walfile.MagicLittleEndian, walfile.FormatVersion, 4096, 0, 1, 2, nil)
		if f, err := walfile.Parse(hdrOnly); err != nil || len(f.Frames) != 0 || f.ValidFrames != 0 {
			problems = append(problems, fmt.Sprintf("header-only: err=%v", err))
		}
		// (d) Short 1..31 byte WAL -> ErrWALIgnored.
		if _, err := walfile.Parse(hdrOnly[:31]); !errors.Is(err, walfile.ErrWALIgnored) {
			problems = append(problems, fmt.Sprintf("31-byte wal err=%v want ErrWALIgnored", err))
		}
		// (e) Bad magic -> ErrWALIgnored.
		badMagic := bytes.Clone(hdrOnly)
		badMagic[0] ^= 0xff
		if _, err := walfile.Parse(badMagic); !errors.Is(err, walfile.ErrWALIgnored) {
			problems = append(problems, fmt.Sprintf("bad-magic err=%v want ErrWALIgnored", err))
		}
		// (f) Invalid page sizes (0, 256, 3000, 131072) -> ErrWALIgnored.
		for _, badPS := range []uint32{0, 256, 3000, 131072} {
			raw, _ := buildSyntheticWAL(walfile.MagicLittleEndian, walfile.FormatVersion, badPS, 0, 1, 2, nil)
			if _, err := walfile.Parse(raw); !errors.Is(err, walfile.ErrWALIgnored) {
				problems = append(problems, fmt.Sprintf("bad-pagesize %d err=%v", badPS, err))
			}
		}
		// (g) Bad header checksum -> ErrWALIgnored.
		badCksum := bytes.Clone(hdrOnly)
		badCksum[31] ^= 0x01
		if _, err := walfile.Parse(badCksum); !errors.Is(err, walfile.ErrWALIgnored) {
			problems = append(problems, fmt.Sprintf("bad-hdr-cksum err=%v", err))
		}
		// (h) Unsupported version 3007001 -> ErrUnsupportedVersion.
		badVer, _ := buildSyntheticWAL(walfile.MagicLittleEndian, 3007001, 4096, 0, 1, 2, nil)
		if _, err := walfile.Parse(badVer); !errors.Is(err, walfile.ErrUnsupportedVersion) {
			problems = append(problems, fmt.Sprintf("bad-version err=%v want ErrUnsupportedVersion", err))
		}
		// (i) Frame with page number 0 -> FramePageZero stops log.
		pg0WAL, _ := buildSyntheticWAL(walfile.MagicLittleEndian, walfile.FormatVersion, 4096, 0, 1, 2, []synthFrame{
			{page: 0, commitSize: 1, data: make([]byte, 4096)},
		})
		if f, err := walfile.Parse(pg0WAL); err != nil || f.ValidFrames != 0 || f.StopReason() != walfile.FramePageZero {
			problems = append(problems, fmt.Sprintf("page-zero: err=%v valid=%d stop=%v", err, f.ValidFrames, f.StopReason()))
		}
		// (j) Real WAL + 100 trailing partial-frame bytes -> PartialFrameBytes==100, Replay matches SQLite E1.
		pDir := filepath.Join(scratch, "ins04-partial")
		if err := os.MkdirAll(filepath.Join(pDir, "copy"), 0o755); err != nil {
			fatal("mkdir ins04-partial", err)
		}
		pDB := filepath.Join(pDir, "r.db")
		r := recipe.Default()
		if err := r.Build(ctx, pDB, 0); err != nil {
			fatal("build ins04-partial", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, pDB, opts)
		if err != nil {
			fatal("open ins04-partial", err)
		}
		if err := r.ApplyStep(ctx, db, 1); err != nil {
			fatal("step ins04-partial", err)
		}
		copyDir := filepath.Join(pDir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: pDB, To: filepath.Join(copyDir, "r.db")},
			{From: pDB + "-wal", To: filepath.Join(copyDir, "r.db-wal")},
		}); err != nil {
			fatal("copy ins04-partial", err)
		}
		db.Close()
		walRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db-wal"))
		if err != nil {
			fatal("read wal ins04-partial", err)
		}
		walWithPartial := append(bytes.Clone(walRaw), bytes.Repeat([]byte{0xde}, 100)...)
		pf, err := walfile.Parse(walWithPartial)
		if err != nil || pf.PartialFrameBytes != 100 || pf.ValidFrames != len(pf.Frames) {
			problems = append(problems, fmt.Sprintf("partial parse: err=%v partial=%d", err, pf.PartialFrameBytes))
		}
		// Compare walfile.Replay vs SQLite opening the same base + partial-tailed WAL.
		baseRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db"))
		if err != nil {
			fatal("read base ins04-partial", err)
		}
		oursBytes, rep, err := walfile.Replay(baseRaw, walWithPartial)
		if err != nil || rep.PartialFrameBytes != 100 {
			problems = append(problems, fmt.Sprintf("partial replay: err=%v partial=%d", err, rep.PartialFrameBytes))
		}
		recDir := filepath.Join(pDir, "sqlite-rec")
		if err := os.MkdirAll(recDir, 0o755); err != nil {
			fatal("mkdir rec", err)
		}
		recDB := filepath.Join(recDir, "r.db")
		if err := os.WriteFile(recDB, baseRaw, 0o644); err != nil {
			fatal("write rec base", err)
		}
		if err := os.WriteFile(recDB+"-wal", walWithPartial, 0o644); err != nil {
			fatal("write rec wal", err)
		}
		sqDB, err := r.Open(ctx, recDB)
		if err != nil {
			problems = append(problems, fmt.Sprintf("sqlite partial open: %v", err))
		} else {
			if _, err := sqlitex.Checkpoint(ctx, sqDB, "", sqlitex.CheckpointTruncate); err != nil {
				problems = append(problems, fmt.Sprintf("sqlite partial ckpt: %v", err))
			}
			sqDB.Close()
			sqBytes, err := os.ReadFile(recDB)
			if err != nil || !bytes.Equal(oursBytes, sqBytes) {
				problems = append(problems, fmt.Sprintf("partial replay != sqlite recovery (err=%v)", err))
			}
		}

		report("INS-04",
			"synthetic WAL edge cases: big-endian magic 0x377f0683 (+showwal), empty/header-only, ignored headers, bad version, page 0, 100 trailing partial bytes",
			"all header/frame error classes match walIndexRecover; big-endian matches showwal; partial tail matches SQLite E1",
			fmt.Sprintf("checked=10 cases, problems=%v", problems),
			len(problems) == 0)
	}

	// INS-05a: Real multi-block -shm (> 4062 WAL frames crossing block 0 -> block 1).
	{
		dir := filepath.Join(scratch, "ins05a")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir ins05a", err)
		}
		dbPath := filepath.Join(dir, "big.db")
		opts := sqlitex.DefaultOptions()
		opts.File.PageSize = 512
		opts.Conn.WALAutocheckpoint = 0
		opts.Conn.Synchronous = sqlitex.SyncOff
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open ins05a", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, pad BLOB)"); err != nil {
			fatal("create ins05a", err)
		}
		// Write enough rows in 10 commits on 512-byte pages to exceed 4062 WAL frames (~2.3 MB).
		pad := bytes.Repeat([]byte("X"), 420)
		for batch := 0; batch < 10; batch++ {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				fatal("begin ins05a", err)
			}
			for j := 0; j < 440; j++ {
				if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, pad) VALUES (?, ?)", batch*1000+j, pad); err != nil {
					tx.Rollback()
					fatal("insert ins05a", err)
				}
			}
			if err := tx.Commit(); err != nil {
				fatal("commit ins05a", err)
			}
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, walfile.DatabaseFileSet(dbPath, copyDir, "big.db")); err != nil {
			fatal("copy ins05a", err)
		}
		db.Close()

		wf, err := walfile.ParseFile(filepath.Join(copyDir, "big.db-wal"))
		if err != nil {
			fatal("parse wal ins05a", err)
		}
		idx, err := walfile.ParseIndexFile(filepath.Join(copyDir, "big.db-shm"))
		if err != nil {
			fatal("parse shm ins05a", err)
		}
		pages, fpErr := idx.FramePages(idx.Header().MaxFrame)
		mismatches := 0
		if fpErr == nil && len(pages) == len(wf.Frames) {
			for i, pg := range pages {
				if pg != wf.Frames[i].Page {
					mismatches++
				}
			}
		}
		held := len(wf.Frames) > walfile.FramesInFirstIndexBlock &&
			idx.Size >= 2*walfile.IndexBlockSize &&
			fpErr == nil &&
			len(pages) == len(wf.Frames) &&
			mismatches == 0
		report("INS-05a",
			"real 512-byte-page WAL exceeding 4062 frames (multi-block -shm crossing block 0 -> block 1)",
			"Index.Size >= 65536 and FramePages(mxFrame) matches every WAL frame page number across the 4062/4063 boundary",
			fmt.Sprintf("frames=%d shm_bytes=%d mxFrame=%d fp_err=%v mismatches=%d", len(wf.Frames), idx.Size, idx.Header().MaxFrame, fpErr, mismatches),
			held)
	}

	// INS-05b: Torn -shm header write (copy 1 bytes 0..47 zeroed while copy 2 bytes 48..95 are valid).
	// walIndexWriteHdr (wal.c:942-954) writes copy 2 first, then barrier, then copy 1;
	// so copy 1 zero + copy 2 valid is the exact initial torn-write state.
	{
		dir := filepath.Join(scratch, "ins05b")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir ins05b", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default()
		if err := r.Build(ctx, dbPath, 0); err != nil {
			fatal("build ins05b", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open ins05b", err)
		}
		if err := r.ApplyStep(ctx, db, 1); err != nil {
			fatal("step ins05b", err)
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{{From: dbPath + "-shm", To: filepath.Join(copyDir, "r.db-shm")}}); err != nil {
			fatal("copy ins05b", err)
		}
		db.Close()

		shmRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db-shm"))
		if err != nil {
			fatal("read shm ins05b", err)
		}
		// Zero copy 1 (bytes 0..47) while preserving valid copy 2 (bytes 48..95).
		torn := bytes.Clone(shmRaw)
		clear(torn[:walfile.IndexHeaderSize])
		idx, parseErr := walfile.ParseIndex(torn)
		detected := parseErr != nil || (idx != nil && !idx.HeadersAgree())
		got := fmt.Sprintf("err=%v", parseErr)
		if idx != nil {
			got = fmt.Sprintf("err=%v initialized=%v headersAgree=%v hdr0.mxFrame=%d hdr1.mxFrame=%d",
				parseErr, idx.Initialized, idx.HeadersAgree(), idx.Headers[0].MaxFrame, idx.Headers[1].MaxFrame)
		}
		report("INS-05b",
			"torn -shm header with copy 1 (bytes 0..47) zeroed and copy 2 (bytes 48..95) populated (midpoint of walIndexWriteHdr)",
			"ParseIndex detects the torn header (error or HeadersAgree()==false)",
			got,
			detected)
	}

	// INS-06: Crash-safety fuzz across walfile parsers, replayer, diff, and showwal parser.
	// Scaled to 5,000 deterministic seeded mutations so the suite finishes well under 10 minutes.
	{
		seedDir := filepath.Join(scratch, "ins06-seed")
		run, err := buildPageSizeWAL(ctx, seedDir, 512)
		if err != nil {
			fatal("ins06 seed", err)
		}
		baseBytes, err := os.ReadFile(filepath.Join(seedDir, "copy", "db.sqlite"))
		if err != nil {
			fatal("read ins06 base", err)
		}
		shmBytes, err := os.ReadFile(filepath.Join(seedDir, "copy", "db.sqlite-shm"))
		if err != nil {
			fatal("read ins06 shm", err)
		}
		sampleShowwal := "Pagesize: 512\nAvailable pages: 1..2\nFrame    1:      1      0 0x11111111,22222222 0x33333333,44444444\n"

		rng := rand.New(rand.NewPCG(0x50505050, 0x05050505))
		const iterations = 5000
		panics := 0
		var firstPanic string

		mutate := func(src []byte) []byte {
			if len(src) == 0 {
				return nil
			}
			buf := bytes.Clone(src)
			switch rng.IntN(5) {
			case 0: // single bit flip
				i := rng.IntN(len(buf))
				buf[i] ^= 1 << uint(rng.IntN(8))
			case 1: // truncate
				buf = buf[:rng.IntN(len(buf))]
			case 2: // multi-byte smash
				start := rng.IntN(len(buf))
				n := min(1+rng.IntN(16), len(buf)-start)
				for j := 0; j < n; j++ {
					buf[start+j] = byte(rng.Uint32())
				}
			case 3: // smash u32 field in first 128 bytes
				if len(buf) >= 4 {
					off := rng.IntN(min(128, len(buf)-3))
					vals := []uint32{0, 1, 0x7fffffff, 0xffffffff, 65536, 3007000}
					binary.BigEndian.PutUint32(buf[off:], vals[rng.IntN(len(vals))])
				}
			case 4: // pure random bytes
				n := rng.IntN(2048)
				buf = make([]byte, n)
				for i := range buf {
					buf[i] = byte(rng.Uint32())
				}
			}
			return buf
		}

		for i := 0; i < iterations; i++ {
			wMut := mutate(run.walBytes)
			sMut := mutate(shmBytes)
			bMut := mutate(baseBytes)
			func() {
				defer func() {
					if r := recover(); r != nil {
						panics++
						if firstPanic == "" {
							firstPanic = fmt.Sprintf("iter=%d panic=%v", i, r)
						}
					}
				}()
				if wf, err := walfile.Parse(wMut); err == nil && wf != nil {
					_ = wf.Commits()
					_ = wf.Tuples(true)
					_ = wf.Tuples(false)
					_ = wf.DatabasePages()
					_ = wf.StopReason()
					if len(wf.Frames) > 0 {
						_, _ = wf.PageImage(1)
						_, _ = wf.PageImage(len(wf.Frames))
					}
				}
				_, _, _ = walfile.Replay(bMut, wMut)
				if idx, err := walfile.ParseIndex(sMut); err == nil && idx != nil {
					_ = idx.HeadersAgree()
					_ = idx.Header().PageSize()
					_, _ = idx.FramePages(min(idx.Header().MaxFrame, 128))
				}
				_ = walfile.CompareBytes(wMut, run.walBytes, 512)
				swMut := string(mutate([]byte(sampleShowwal)))
				_, _ = walfile.ParseShowwal(swMut)
			}()
		}
		report("INS-06",
			fmt.Sprintf("fuzz walfile Parse/Replay/ParseIndex/CompareBytes/ParseShowwal with %d seeded mutations", iterations),
			"0 panics across all mutations",
			fmt.Sprintf("iterations=%d panics=%d first_panic=%q", iterations, panics, firstPanic),
			panics == 0)
	}

	// INS-07 ("must be able to fail"): Pin enforcement across all 4 ch05 experiments.
	{
		scratchRepo := filepath.Join(scratch, "scratch-repo")
		if err := buildScratchRepo(repoRoot, scratchRepo); err != nil {
			fatal("build scratch repo", err)
		}
		pinPath := filepath.Join(scratchRepo, "sqlite", "PIN")
		pinBytes, err := os.ReadFile(pinPath)
		if err != nil {
			fatal("read PIN", err)
		}
		mutated := strings.ReplaceAll(string(pinBytes),
			"bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc",
			"0000000000000000000000000000000000000000000000000000000000000000")
		if err := os.WriteFile(pinPath, []byte(mutated), 0o644); err != nil {
			fatal("write mutated PIN", err)
		}
		workDir := filepath.Join(scratch, "pin-work")
		exps := []struct {
			name string
			pkg  string
		}{
			{"5.1", "./ch05-wal-transactions-checkpoints/experiments/5.1-wal-inspector"},
			{"5.2", "./ch05-wal-transactions-checkpoints/experiments/5.2-wal-replay"},
			{"5.3", "./ch05-wal-transactions-checkpoints/experiments/5.3-reader-pins-the-wal"},
			{"5.4", "./ch05-wal-transactions-checkpoints/experiments/5.4-checkpoint-modes"},
		}
		var failed []string
		var summary []string
		for _, e := range exps {
			res, err := runGoPkg(ctx, scratchRepo, e.pkg, []string{"COURSE_WORK_DIR=" + workDir})
			if err != nil {
				fatal("run "+e.name, err)
			}
			l1 := firstLine(res.stdout)
			ok := res.exitCode != 0 && strings.HasPrefix(l1, "sqlite_source_id:") && strings.Contains(l1, "NOT the pin")
			summary = append(summary, fmt.Sprintf("%s:exit=%d", e.name, res.exitCode))
			if !ok {
				failed = append(failed, fmt.Sprintf("%s(exit=%d,line1=%q)", e.name, res.exitCode, l1))
			}
		}
		report("INS-07",
			"mutate sqlite/PIN in scratch repo and run ch05 experiments 5.1, 5.2, 5.3, 5.4",
			"all 4 experiments exit non-zero and report sqlite_source_id mismatch on line 1",
			fmt.Sprintf("results=[%s] failures=%v", strings.Join(summary, ", "), failed),
			len(failed) == 0)
	}
}
