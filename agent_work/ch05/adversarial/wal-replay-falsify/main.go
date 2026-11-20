// wal-replay-falsify attacks Experiment 5.2 (Rung R3) and internal/walfile/replay.go:
// damaged last/middle commit frames, stale-generation base detection, applying
// frames past the last commit, shrinking auto_vacuum databases, spill+savepoint
// commits, empty/non-default page-size bases, and Replay/ReplayFiles edge cases.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// sqliteRecoverAndCheckpoint writes (base, wal) into dir (with no -shm so
// SQLite recovers directly from the WAL), opens it with opts, reads user_version
// and integrity_check, runs wal_checkpoint(TRUNCATE), closes, and returns the
// final database bytes plus metadata.
func sqliteRecoverAndCheckpoint(ctx context.Context, dir string, base, wal []byte, opts sqlitex.Options) ([]byte, int, string, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, 0, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, "", err
	}
	dbPath := filepath.Join(dir, "db.sqlite")
	if err := os.WriteFile(dbPath, base, 0o644); err != nil {
		return nil, 0, "", err
	}
	if wal != nil {
		if err := os.WriteFile(dbPath+"-wal", wal, 0o644); err != nil {
			return nil, 0, "", err
		}
	}
	db, err := sqlitex.Open(ctx, dbPath, opts)
	if err != nil {
		return nil, 0, "", fmt.Errorf("open: %w", err)
	}
	var uv int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uv); err != nil {
		db.Close()
		return nil, 0, "", fmt.Errorf("user_version: %w", err)
	}
	var ic string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
		db.Close()
		return nil, 0, "", fmt.Errorf("integrity_check: %w", err)
	}
	if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
		db.Close()
		return nil, 0, "", fmt.Errorf("truncate: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, 0, "", fmt.Errorf("close: %w", err)
	}
	finalBytes, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, 0, "", err
	}
	return finalBytes, uv, ic, nil
}

// inspectDBBytes writes bytes to a temp file and queries SQLite for user_version
// and PRAGMA integrity_check without mutating the database.
func inspectDBBytes(ctx context.Context, dir string, data []byte, opts sqlitex.Options) (int, string, error) {
	if err := os.RemoveAll(dir); err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, "", err
	}
	p := filepath.Join(dir, "inspect.db")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return 0, "", err
	}
	opts.ReadOnly = true
	db, err := sqlitex.Open(ctx, p, opts)
	if err != nil {
		return 0, "", err
	}
	defer db.Close()
	var uv int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&uv); err != nil {
		return 0, "", err
	}
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return uv, "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return uv, "", err
		}
		lines = append(lines, s)
	}
	return uv, strings.Join(lines, "; "), rows.Err()
}

func buildHeaderOnlyWAL(pageSize uint32) []byte {
	hdr := make([]byte, walfile.HeaderSize)
	be := binary.BigEndian
	be.PutUint32(hdr[0:], walfile.MagicLittleEndian)
	be.PutUint32(hdr[4:], walfile.FormatVersion)
	be.PutUint32(hdr[8:], pageSize)
	be.PutUint32(hdr[12:], 0)
	be.PutUint32(hdr[16:], 0x12345678)
	be.PutUint32(hdr[20:], 0x9abcdef0)
	ck, err := walfile.ChecksumBytes(false, hdr[:24], walfile.Checksum{})
	if err != nil {
		fatal("header checksum", err)
	}
	be.PutUint32(hdr[24:], ck[0])
	be.PutUint32(hdr[28:], ck[1])
	return hdr
}

func main() {
	if walfile.ServeLiveCopyChild(os.Args) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch05", "wal-replay-falsify")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// REP-01 (OUTLINE 5.2 bullet 1): Flip one byte in the last commit's frames
	// (3 positions) plus a middle commit (commit 3 of 5).
	{
		dir := filepath.Join(scratch, "rep01")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir rep01", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(5)
		if err := r.Build(ctx, dbPath, 0); err != nil {
			fatal("build rep01", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open rep01", err)
		}
		for k := 1; k <= 5; k++ {
			if err := r.ApplyStep(ctx, db, k); err != nil {
				db.Close()
				fatal("step rep01", err)
			}
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: dbPath, To: filepath.Join(copyDir, "r.db")},
			{From: dbPath + "-wal", To: filepath.Join(copyDir, "r.db-wal")},
		}); err != nil {
			db.Close()
			fatal("copy rep01", err)
		}
		db.Close()

		baseRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db"))
		if err != nil {
			fatal("read base rep01", err)
		}
		walRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db-wal"))
		if err != nil {
			fatal("read wal rep01", err)
		}
		parsed, err := walfile.Parse(walRaw)
		if err != nil {
			fatal("parse rep01", err)
		}
		commits := parsed.Commits()
		if len(commits) != 5 {
			fatal("rep01 commits", fmt.Errorf("got %d want 5", len(commits)))
		}
		c3 := commits[2]
		c5 := commits[4]

		type flipCase struct {
			name     string
			offset   int64
			wantStep int
			wantIgn  int
		}
		cases := []flipCase{
			{
				name:     "last-commit-first-frame-page",
				offset:   parsed.Frames[c5.FirstFrame-1].Offset + walfile.FrameHeaderSize + 100,
				wantStep: 4,
				wantIgn:  len(parsed.Frames) - (c5.FirstFrame - 1),
			},
			{
				name:     "last-commit-final-frame-page",
				offset:   parsed.Frames[c5.LastFrame-1].Offset + walfile.FrameHeaderSize + 200,
				wantStep: 4,
				wantIgn:  len(parsed.Frames) - (c5.FirstFrame - 1),
			},
			{
				name:     "last-commit-first-frame-pgno",
				offset:   parsed.Frames[c5.FirstFrame-1].Offset + 3,
				wantStep: 4,
				wantIgn:  len(parsed.Frames) - (c5.FirstFrame - 1),
			},
			{
				name:     "middle-commit-3-first-frame-page",
				offset:   parsed.Frames[c3.FirstFrame-1].Offset + walfile.FrameHeaderSize + 64,
				wantStep: 2,
				wantIgn:  len(parsed.Frames) - (c3.FirstFrame - 1),
			},
		}

		var problems []string
		for _, tc := range cases {
			damaged := bytes.Clone(walRaw)
			damaged[tc.offset] ^= 0x01
			ours, rep, err := walfile.Replay(baseRaw, damaged)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s replay err: %v", tc.name, err))
				continue
			}
			theirs, sqUV, sqIC, err := sqliteRecoverAndCheckpoint(ctx, filepath.Join(dir, "sq-"+tc.name), baseRaw, damaged, r.SQLiteOptions())
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s sqlite err: %v", tc.name, err))
				continue
			}
			oursUV, oursIC, err := inspectDBBytes(ctx, filepath.Join(dir, "in-"+tc.name), ours, r.SQLiteOptions())
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s inspect err: %v", tc.name, err))
				continue
			}
			if !bytes.Equal(ours, theirs) {
				problems = append(problems, fmt.Sprintf("%s E1 mismatch (ours=%s sqlite=%s)", tc.name, sha256Hex(ours), sha256Hex(theirs)))
			}
			if oursUV != tc.wantStep || sqUV != tc.wantStep || oursIC != "ok" || sqIC != "ok" {
				problems = append(problems, fmt.Sprintf("%s uv=%d/%d want=%d ic=%q/%q", tc.name, oursUV, sqUV, tc.wantStep, oursIC, sqIC))
			}
			if rep.IgnoredFrameCount() != tc.wantIgn {
				problems = append(problems, fmt.Sprintf("%s ignored=%d want=%d", tc.name, rep.IgnoredFrameCount(), tc.wantIgn))
			}
		}
		report("REP-01",
			"flip 1 bit in last commit (3 locations) and middle commit (commit 3/5); compare walfile.Replay vs SQLite recovery",
			"E1 byte-identical to SQLite recovery in all 4 cases, landing at step 4 (last) and step 2 (middle) with integrity_check=ok",
			fmt.Sprintf("tested=4 bit-flip cases, problems=%v", problems),
			len(problems) == 0)
	}

	// REP-02 (OUTLINE 5.2 bullet 2): Replay a generation-2 WAL onto a stale
	// generation-0 base. Outline 5.2 requires: "must be detected — the chapter's
	// rule that base and WAL travel together".
	{
		dir := filepath.Join(scratch, "rep02")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir rep02", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(8)
		if err := r.Build(ctx, dbPath, 0); err != nil {
			fatal("build rep02", err)
		}
		// Save generation-0 base file before any commits.
		gen0Base, err := os.ReadFile(dbPath)
		if err != nil {
			fatal("read gen0Base", err)
		}

		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open rep02", err)
		}
		// Generation 1: commit steps 1..5 and backfill them into the base file.
		for k := 1; k <= 5; k++ {
			if err := r.ApplyStep(ctx, db, k); err != nil {
				db.Close()
				fatal("gen1 step", err)
			}
		}
		if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointPassive); err != nil {
			db.Close()
			fatal("passive rep02", err)
		}
		// Generation 2: commit steps 6..8 (restarts WAL at frame 1 with new salts & ckpt-seq=1).
		for k := 6; k <= 8; k++ {
			if err := r.ApplyStep(ctx, db, k); err != nil {
				db.Close()
				fatal("gen2 step", err)
			}
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: dbPath + "-wal", To: filepath.Join(copyDir, "gen2-wal")},
		}); err != nil {
			db.Close()
			fatal("copy gen2 wal", err)
		}
		db.Close()

		gen2WAL, err := os.ReadFile(filepath.Join(copyDir, "gen2-wal"))
		if err != nil {
			fatal("read gen2 wal", err)
		}
		wf, err := walfile.Parse(gen2WAL)
		if err != nil {
			fatal("parse gen2 wal", err)
		}
		replayed, rep, replayErr := walfile.Replay(gen0Base, gen2WAL)
		if replayErr != nil {
			report("REP-02",
				"replay generation-2 WAL (steps 6-8, ckpt-seq=1) onto stale generation-0 base (step 0)",
				"stale-generation base mismatch is detected and rejected by walfile.Replay (OUTLINE 5.2)",
				fmt.Sprintf("rejected with error: %v", replayErr),
				true)
		} else {
			uv, ic, inspErr := inspectDBBytes(ctx, filepath.Join(dir, "inspect-stale"), replayed, r.SQLiteOptions())
			icSummary := ic
			if len(icSummary) > 100 {
				icSummary = icSummary[:100] + "..."
			}
			report("REP-02",
				"replay generation-2 WAL (steps 6-8, ckpt-seq=1) onto stale generation-0 base (step 0)",
				"stale-generation base mismatch is detected and rejected by walfile.Replay (OUTLINE 5.2)",
				fmt.Sprintf("Replay returned err=nil (wal_ckpt_seq=%d applied=%d out=%dB); opening result: uv=%d inspect_err=%v integrity_check=%q",
					wf.Header.CheckpointSequence, rep.AppliedFrames, len(replayed), uv, inspErr, icSummary),
				false)
		}
	}

	// REP-03 (OUTLINE 5.2 bullet 3): Apply frames past the last commit ("must be
	// able to fail") and show the result contains rolled-back sentinel data.
	{
		dir := filepath.Join(scratch, "rep03")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir rep03", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(11)
		if err := r.Build(ctx, dbPath, 8); err != nil {
			fatal("build rep03", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		opts.Conn.CacheSize = 10
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open rep03", err)
		}
		for k := 9; k <= 10; k++ {
			if err := r.ApplyStep(ctx, db, k); err != nil {
				db.Close()
				fatal("step rep03", err)
			}
		}
		const sentinel = "ROLLED_BACK_SENTINEL_PAYLOAD_999"
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			fatal("begin rep03", err)
		}
		// Insert sentinel rows and update enough rows to spill pages to the WAL under cache_size=10.
		for i := 0; i < 60; i++ {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO events(id, account_id, kind, amount, note) VALUES (?, 1, 'deposit', 777, ?)",
				900000+i, fmt.Sprintf("%s-%03d-%s", sentinel, i, strings.Repeat("Z", 180))); err != nil {
				tx.Rollback()
				db.Close()
				fatal("insert sentinel", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE events SET note = upper(note)"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("update spill", err)
		}
		// Roll back: spilled frames remain in the -wal file past LastCommitFrame.
		if err := tx.Rollback(); err != nil {
			db.Close()
			fatal("rollback rep03", err)
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: dbPath, To: filepath.Join(copyDir, "r.db")},
			{From: dbPath + "-wal", To: filepath.Join(copyDir, "r.db-wal")},
		}); err != nil {
			db.Close()
			fatal("copy rep03", err)
		}
		if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
			db.Close()
			fatal("truncate rep03", err)
		}
		db.Close()

		baseRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db"))
		if err != nil {
			fatal("read base rep03", err)
		}
		walRaw, err := os.ReadFile(filepath.Join(copyDir, "r.db-wal"))
		if err != nil {
			fatal("read wal rep03", err)
		}
		sqliteFinal, err := os.ReadFile(dbPath)
		if err != nil {
			fatal("read sqliteFinal rep03", err)
		}

		// (a) Proper walfile.Replay stops at LastCommitFrame.
		properOut, rep, err := walfile.Replay(baseRaw, walRaw)
		if err != nil {
			fatal("proper replay rep03", err)
		}
		properHasSentinel := bytes.Contains(properOut, []byte(sentinel))
		properMatchesSQLite := bytes.Equal(properOut, sqliteFinal)

		// (b) Naive broken replayer: applies ALL frames in the WAL, ignoring LastCommitFrame.
		wf, err := walfile.Parse(walRaw)
		if err != nil {
			fatal("parse rep03", err)
		}
		maxPg := wf.DatabasePages()
		for _, fr := range wf.Frames {
			if fr.Page > maxPg {
				maxPg = fr.Page
			}
		}
		ps := wf.PageSize()
		naiveOut := make([]byte, max(int64(len(baseRaw)), int64(maxPg)*int64(ps)))
		copy(naiveOut, baseRaw)
		for _, fr := range wf.Frames {
			if fr.Page == 0 {
				continue
			}
			img, err := wf.PageImage(fr.Number)
			if err != nil {
				fatal("page image rep03", err)
			}
			copy(naiveOut[int64(fr.Page-1)*int64(ps):], img)
		}
		naiveHasSentinel := bytes.Contains(naiveOut, []byte(sentinel))
		_, naiveIC, naiveErr := inspectDBBytes(ctx, filepath.Join(dir, "inspect-naive"), naiveOut, r.SQLiteOptions())

		held := !properHasSentinel && properMatchesSQLite && rep.IgnoredFrameCount() > 0 && naiveHasSentinel && !bytes.Equal(naiveOut, sqliteFinal)
		report("REP-03",
			"compare proper walfile.Replay vs naive replayer applying spilled rolled-back frames past LastCommitFrame",
			"proper replay excludes sentinel and equals SQLite E1; naive replay leaks rolled-back sentinel bytes (and differs/corrupts)",
			fmt.Sprintf("spilled=%d proper_sentinel=%v proper_E1=%v naive_sentinel=%v naive_ic=%q(err=%v)",
				len(wf.Frames)-wf.LastCommitFrame, properHasSentinel, properMatchesSQLite, naiveHasSentinel, naiveIC, naiveErr),
			held)
	}

	// REP-04: Shrinking database via auto_vacuum=FULL + DELETE across commits in one WAL.
	{
		dir := filepath.Join(scratch, "rep04")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir rep04", err)
		}
		dbPath := filepath.Join(dir, "shrink.db")
		opts := sqlitex.DefaultOptions()
		opts.File.AutoVacuum = sqlitex.AutoVacuumFull
		opts.Conn.WALAutocheckpoint = 0
		initDB, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("init rep04", err)
		}
		if _, err := initDB.ExecContext(ctx, "CREATE TABLE blobtab (id INTEGER PRIMARY KEY, body BLOB)"); err != nil {
			initDB.Close()
			fatal("create rep04", err)
		}
		if err := initDB.Close(); err != nil {
			fatal("close init rep04", err)
		}

		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open rep04", err)
		}
		// Commit 1: grow DB to ~70+ pages.
		tx1, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			fatal("tx1 rep04", err)
		}
		bigBlob := bytes.Repeat([]byte("S"), 1500)
		for i := 1; i <= 180; i++ {
			if _, err := tx1.ExecContext(ctx, "INSERT INTO blobtab(id, body) VALUES (?, ?)", i, bigBlob); err != nil {
				tx1.Rollback()
				db.Close()
				fatal("insert rep04", err)
			}
		}
		if err := tx1.Commit(); err != nil {
			db.Close()
			fatal("commit1 rep04", err)
		}
		var peakPages int
		if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&peakPages); err != nil {
			db.Close()
			fatal("peak page_count", err)
		}
		// Commit 2: delete 170 rows -> auto_vacuum=FULL shrinks page_count sharply.
		tx2, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			fatal("tx2 rep04", err)
		}
		if _, err := tx2.ExecContext(ctx, "DELETE FROM blobtab WHERE id > 10"); err != nil {
			tx2.Rollback()
			db.Close()
			fatal("delete rep04", err)
		}
		if err := tx2.Commit(); err != nil {
			db.Close()
			fatal("commit2 rep04", err)
		}
		var finalPages int
		if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&finalPages); err != nil {
			db.Close()
			fatal("final page_count", err)
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: dbPath, To: filepath.Join(copyDir, "shrink.db")},
			{From: dbPath + "-wal", To: filepath.Join(copyDir, "shrink.db-wal")},
		}); err != nil {
			db.Close()
			fatal("copy rep04", err)
		}
		if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
			db.Close()
			fatal("truncate rep04", err)
		}
		db.Close()

		replayedPath := filepath.Join(copyDir, "replayed.db")
		rep, err := walfile.ReplayFiles(filepath.Join(copyDir, "shrink.db"), filepath.Join(copyDir, "shrink.db-wal"), replayedPath)
		if err != nil {
			fatal("replay rep04", err)
		}
		cmp, err := equal.CompareFiles(replayedPath, dbPath, 4096)
		if err != nil {
			fatal("compare rep04", err)
		}
		held := peakPages > finalPages && rep.SkippedBeyondSize > 0 && int(rep.DatabasePages) == finalPages && cmp.Equal
		report("REP-04",
			"auto_vacuum=FULL database grows in commit 1 and shrinks in commit 2 within one WAL",
			"walfile.Replay skips pages beyond final commit size, truncates output, and matches SQLite TRUNCATE E1",
			fmt.Sprintf("peak_pages=%d final_pages=%d skipped=%d E1_equal=%v", peakPages, finalPages, rep.SkippedBeyondSize, cmp.Equal),
			held)
	}

	// REP-05: Committed transaction with cache-spill frame overwrites + SAVEPOINT / ROLLBACK TO.
	{
		dir := filepath.Join(scratch, "rep05")
		if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
			fatal("mkdir rep05", err)
		}
		dbPath := filepath.Join(dir, "r.db")
		r := recipe.Default().WithSteps(11)
		if err := r.Build(ctx, dbPath, 8); err != nil {
			fatal("build rep05", err)
		}
		opts := r.SQLiteOptions()
		opts.Conn.WALAutocheckpoint = 0
		opts.Conn.CacheSize = 5
		db, err := sqlitex.Open(ctx, dbPath, opts)
		if err != nil {
			fatal("open rep05", err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			fatal("begin rep05", err)
		}
		// Trigger repeated page spills and overwrites, then a rolled-back savepoint, then commit.
		if _, err := tx.ExecContext(ctx, "UPDATE events SET note = note || '-v1'"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("u1 rep05", err)
		}
		if _, err := tx.ExecContext(ctx, "SAVEPOINT sp1"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("sp1 rep05", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE events SET note = 'SAVEPOINT_SHOULD_VANISH-' || note"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("u2 rep05", err)
		}
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO sp1"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("rb sp1 rep05", err)
		}
		if _, err := tx.ExecContext(ctx, "RELEASE sp1"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("rel sp1 rep05", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE events SET note = note || '-v2'"); err != nil {
			tx.Rollback()
			db.Close()
			fatal("u3 rep05", err)
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			fatal("commit rep05", err)
		}
		copyDir := filepath.Join(dir, "copy")
		if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
			{From: dbPath, To: filepath.Join(copyDir, "r.db")},
			{From: dbPath + "-wal", To: filepath.Join(copyDir, "r.db-wal")},
		}); err != nil {
			db.Close()
			fatal("copy rep05", err)
		}
		if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
			db.Close()
			fatal("truncate rep05", err)
		}
		db.Close()

		wf, err := walfile.ParseFile(filepath.Join(copyDir, "r.db-wal"))
		if err != nil {
			fatal("parse rep05", err)
		}
		replayedPath := filepath.Join(copyDir, "replayed.db")
		rep, err := walfile.ReplayFiles(filepath.Join(copyDir, "r.db"), filepath.Join(copyDir, "r.db-wal"), replayedPath)
		if err != nil {
			fatal("replay rep05", err)
		}
		cmp, err := equal.CompareFiles(replayedPath, dbPath, 4096)
		if err != nil {
			fatal("compare rep05", err)
		}
		replayedRaw, _ := os.ReadFile(replayedPath)
		hasSavepointGhost := bytes.Contains(replayedRaw, []byte("SAVEPOINT_SHOULD_VANISH"))
		held := wf.ValidFrames == len(wf.Frames) && rep.IgnoredFrameCount() == 0 && cmp.Equal && !hasSavepointGhost
		report("REP-05",
			"committed transaction with cache_size=5 spills/in-place overwrites + SAVEPOINT/ROLLBACK TO/RELEASE",
			"walRewriteChecksums keeps full WAL valid, walfile.Replay matches SQLite TRUNCATE E1 without savepoint ghost",
			fmt.Sprintf("valid=%d/%d applied=%d E1_equal=%v savepoint_ghost=%v", wf.ValidFrames, len(wf.Frames), rep.AppliedFrames, cmp.Equal, hasSavepointGhost),
			held)
	}

	// REP-06: Replay from 0-byte base (brand-new WAL DB where base is untouched/empty)
	// plus replay at non-default page sizes 512 and 65536.
	{
		var problems []string
		// (a) Brand-new WAL DB created in one open without checkpointing: base file
		// on disk starts empty/small while all schema+data live in WAL; also test
		// explicit empty []byte base when WAL contains every page 1..N.
		for _, ps := range []int{512, 65536} {
			dir := filepath.Join(scratch, fmt.Sprintf("rep06-%d", ps))
			if err := os.MkdirAll(filepath.Join(dir, "copy"), 0o755); err != nil {
				fatal("mkdir rep06", err)
			}
			dbPath := filepath.Join(dir, "db.sqlite")
			opts := sqlitex.DefaultOptions()
			opts.File.PageSize = ps
			opts.Conn.WALAutocheckpoint = 0
			db, err := sqlitex.Open(ctx, dbPath, opts)
			if err != nil {
				problems = append(problems, fmt.Sprintf("ps=%d open: %v", ps, err))
				continue
			}
			if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v BLOB)"); err != nil {
				db.Close()
				problems = append(problems, fmt.Sprintf("ps=%d create: %v", ps, err))
				continue
			}
			val := bytes.Repeat([]byte("P"), ps/2)
			for i := 1; i <= 12; i++ {
				if _, err := db.ExecContext(ctx, "INSERT INTO t(id, v) VALUES (?, ?)", i, val); err != nil {
					db.Close()
					problems = append(problems, fmt.Sprintf("ps=%d insert: %v", ps, err))
					break
				}
			}
			copyDir := filepath.Join(dir, "copy")
			if _, err := walfile.CopyLiveFiles(ctx, []walfile.CopyPair{
				{From: dbPath, To: filepath.Join(copyDir, "db.sqlite")},
				{From: dbPath + "-wal", To: filepath.Join(copyDir, "db.sqlite-wal")},
			}); err != nil {
				db.Close()
				fatal("copy rep06", err)
			}
			if _, err := sqlitex.Checkpoint(ctx, db, "", sqlitex.CheckpointTruncate); err != nil {
				db.Close()
				problems = append(problems, fmt.Sprintf("ps=%d truncate: %v", ps, err))
				continue
			}
			db.Close()

			baseRaw, _ := os.ReadFile(filepath.Join(copyDir, "db.sqlite"))
			walRaw, _ := os.ReadFile(filepath.Join(copyDir, "db.sqlite-wal"))
			sqliteFinal, _ := os.ReadFile(dbPath)
			ours, _, err := walfile.Replay(baseRaw, walRaw)
			if err != nil {
				problems = append(problems, fmt.Sprintf("ps=%d replay err: %v", ps, err))
			} else if !bytes.Equal(ours, sqliteFinal) {
				problems = append(problems, fmt.Sprintf("ps=%d replay != sqlite E1", ps))
			}
			// If every page 1..N was written into the WAL (base only had page 1 from
			// journal_mode=wal setup, or base was 0), verify empty-base replay when page 1 is in WAL.
			wf, _ := walfile.Parse(walRaw)
			seen := map[uint32]bool{}
			for _, fr := range wf.CommittedFrames() {
				seen[fr.Page] = true
			}
			allInWAL := len(seen) == int(wf.DatabasePages())
			if allInWAL {
				fromZero, _, err := walfile.Replay(nil, walRaw)
				if err != nil || !bytes.Equal(fromZero, sqliteFinal) {
					problems = append(problems, fmt.Sprintf("ps=%d empty-base replay mismatch (err=%v)", ps, err))
				}
			}
		}
		report("REP-06",
			"walfile.Replay at page sizes 512 and 65536 (including from 0-byte empty base when WAL covers all pages)",
			"E1 byte equality with SQLite wal_checkpoint(TRUNCATE) at both extreme page sizes and from empty base",
			fmt.Sprintf("tested=[512, 65536], problems=%v", problems),
			len(problems) == 0)
	}

	// REP-07a: Replay validation when WAL has 0 committed frames (header-only WAL)
	// paired with (1) mismatched page size (base=4096, WAL=8192) and (2) non-page-multiple
	// base size (5000 bytes with valid SQLite header).
	{
		dir := filepath.Join(scratch, "rep07a")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir rep07a", err)
		}
		dbPath := filepath.Join(dir, "base.db")
		if err := recipe.Default().Build(ctx, dbPath, 0); err != nil {
			fatal("build rep07a", err)
		}
		base4096, err := os.ReadFile(dbPath)
		if err != nil {
			fatal("read base4096", err)
		}
		wal8192Empty := buildHeaderOnlyWAL(8192)
		wal4096Empty := buildHeaderOnlyWAL(4096)
		badLenBase := append(bytes.Clone(base4096), make([]byte, 904)...) // 16384+904 not a multiple of 4096

		_, _, mismatchErr := walfile.Replay(base4096, wal8192Empty)
		_, _, badLenErr := walfile.Replay(badLenBase, wal4096Empty)
		held := mismatchErr != nil && badLenErr != nil
		report("REP-07a",
			"walfile.Replay with 0-commit header-only WAL paired with mismatched page size (4096 vs 8192) and non-page-multiple base",
			"Replay rejects both with ErrPageSizeMismatch and ErrBaseSize even when LastCommitFrame==0",
			fmt.Sprintf("mismatch_err=%v bad_base_len_err=%v", mismatchErr, badLenErr),
			held)
	}

	// REP-07b: ReplayFiles hygiene when outPath+".replay-tmp" already exists.
	{
		dir := filepath.Join(scratch, "rep07b")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("mkdir rep07b", err)
		}
		basePath := filepath.Join(dir, "base.db")
		if err := recipe.Default().Build(ctx, basePath, 0); err != nil {
			fatal("build rep07b", err)
		}
		walPath := filepath.Join(dir, "base.db-wal") // absent -> 0 frames
		outPath := filepath.Join(dir, "out.db")
		tmpPath := outPath + ".replay-tmp"
		sentinel := []byte("PRE_EXISTING_TMP_DO_NOT_DELETE")
		if err := os.WriteFile(tmpPath, sentinel, 0o644); err != nil {
			fatal("write pre-existing tmp", err)
		}
		_, err := walfile.ReplayFiles(basePath, walPath, outPath)
		afterBytes, statErr := os.ReadFile(tmpPath)
		preserved := statErr == nil && bytes.Equal(afterBytes, sentinel)
		held := err != nil && preserved
		report("REP-07b",
			"call walfile.ReplayFiles when outPath+\".replay-tmp\" already exists with sentinel content",
			"ReplayFiles returns an error and preserves the pre-existing .replay-tmp file untouched",
			fmt.Sprintf("replay_err=%v tmp_still_exists=%v tmp_preserved=%v", err, statErr == nil, preserved),
			held)
	}
}
