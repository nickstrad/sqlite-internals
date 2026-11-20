package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const (
	pageSize       = 4096
	totalFiftyRuns = 50
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

func short(h string) string {
	if len(h) < 16 {
		return h
	}
	return h[:16]
}

// diffSummary compares two closed database files and reports differing page numbers
// plus differing byte offsets on page 1 when page 1 differs.
func diffSummary(a, b string) (equal bool, summary string) {
	da, err := os.ReadFile(a)
	if err != nil {
		fatal("read "+a, err)
	}
	db, err := os.ReadFile(b)
	if err != nil {
		fatal("read "+b, err)
	}
	if bytes.Equal(da, db) {
		return true, "identical"
	}
	pages := max(len(da), len(db)) / pageSize
	var diffPages []int
	var p1Bytes []int
	for p := range pages {
		start, end := p*pageSize, (p+1)*pageSize
		var pa, pb []byte
		if end <= len(da) {
			pa = da[start:end]
		}
		if end <= len(db) {
			pb = db[start:end]
		}
		if bytes.Equal(pa, pb) {
			continue
		}
		diffPages = append(diffPages, p+1)
		if p == 0 && pa != nil && pb != nil {
			for i := range pageSize {
				if pa[i] != pb[i] {
					p1Bytes = append(p1Bytes, i)
				}
			}
		}
	}
	s := fmt.Sprintf("differ(%d pages, first=%d", len(diffPages), diffPages[0])
	if len(da) != len(db) {
		s += fmt.Sprintf(", sizes=%d vs %d", len(da), len(db))
	}
	if len(p1Bytes) > 0 {
		if len(p1Bytes) <= 10 {
			s += fmt.Sprintf(", p1Bytes=%v", p1Bytes)
		} else {
			s += fmt.Sprintf(", p1Bytes=%v...(%d total)", p1Bytes[:10], len(p1Bytes))
		}
	}
	s += ")"
	return false, s
}

func buildWithCustomSessionPragmas(ctx context.Context, r recipe.Recipe, path string, mod func(*sqlitex.Options), extraPragmas []string) (recipe.FileHashes, error) {
	if err := recipe.RemoveDatabase(path); err != nil {
		return recipe.FileHashes{}, err
	}
	plan, err := r.Plan()
	if err != nil {
		return recipe.FileHashes{}, err
	}
	opts := r.SQLiteOptions()
	if mod != nil {
		mod(&opts)
	}
	for _, s := range plan {
		db, err := sqlitex.Open(ctx, path, opts)
		if err != nil {
			return recipe.FileHashes{}, err
		}
		for _, p := range extraPragmas {
			if _, err := db.ExecContext(ctx, p); err != nil {
				db.Close()
				return recipe.FileHashes{}, fmt.Errorf("%s: %w", p, err)
			}
		}
		if err := recipe.ApplyStep(ctx, db, s); err != nil {
			db.Close()
			return recipe.FileHashes{}, err
		}
		if err := db.Close(); err != nil {
			return recipe.FileHashes{}, err
		}
		if err := recipe.CheckAtRest(path); err != nil {
			return recipe.FileHashes{}, err
		}
	}
	return recipe.HashFile(path)
}

func buildOneConnection(ctx context.Context, r recipe.Recipe, path string, secureDeleteMode string, batchMode string, autockpt int) (recipe.FileHashes, error) {
	if err := recipe.RemoveDatabase(path); err != nil {
		return recipe.FileHashes{}, err
	}
	plan, err := r.Plan()
	if err != nil {
		return recipe.FileHashes{}, err
	}
	opts := r.SQLiteOptions()
	opts.Conn.WALAutocheckpoint = autockpt
	if secureDeleteMode == "OFF" {
		opts.Conn.SecureDelete = false
	}
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		return recipe.FileHashes{}, err
	}
	if secureDeleteMode == "FAST" {
		if _, err := db.ExecContext(ctx, "PRAGMA secure_delete=FAST"); err != nil {
			db.Close()
			return recipe.FileHashes{}, err
		}
	}
	switch batchMode {
	case "per-step":
		for _, s := range plan {
			if err := recipe.ApplyStep(ctx, db, s); err != nil {
				db.Close()
				return recipe.FileHashes{}, err
			}
		}
	case "single-tx":
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			db.Close()
			return recipe.FileHashes{}, err
		}
		for _, s := range plan {
			if err := s.ExecIn(ctx, tx); err != nil {
				tx.Rollback()
				db.Close()
				return recipe.FileHashes{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			return recipe.FileHashes{}, err
		}
	case "autocommit":
		for _, s := range plan {
			if err := s.ExecIn(ctx, db); err != nil {
				db.Close()
				return recipe.FileHashes{}, err
			}
		}
	default:
		db.Close()
		return recipe.FileHashes{}, fmt.Errorf("unknown batchMode %q", batchMode)
	}
	if err := db.Close(); err != nil {
		return recipe.FileHashes{}, err
	}
	if err := recipe.CheckAtRest(path); err != nil {
		return recipe.FileHashes{}, err
	}
	return recipe.HashFile(path)
}

func renderSQL(stmt recipe.Statement) (string, error) {
	if len(stmt.Args) == 0 {
		return stmt.SQL + ";", nil
	}
	var out strings.Builder
	argIdx := 0
	for i := 0; i < len(stmt.SQL); i++ {
		if stmt.SQL[i] != '?' {
			out.WriteByte(stmt.SQL[i])
			continue
		}
		if argIdx >= len(stmt.Args) {
			return "", fmt.Errorf("too few args for %q", stmt.SQL)
		}
		switch v := stmt.Args[argIdx].(type) {
		case int64:
			out.WriteString(strconv.FormatInt(v, 10))
		case int:
			out.WriteString(strconv.Itoa(v))
		case string:
			out.WriteByte('\'')
			out.WriteString(strings.ReplaceAll(v, "'", "''"))
			out.WriteByte('\'')
		case []byte:
			out.WriteString("X'")
			out.WriteString(strings.ToUpper(hex.EncodeToString(v)))
			out.WriteByte('\'')
		default:
			return "", fmt.Errorf("unsupported arg type %T", v)
		}
		argIdx++
	}
	if argIdx != len(stmt.Args) {
		return "", fmt.Errorf("unused args for %q: used %d of %d", stmt.SQL, argIdx, len(stmt.Args))
	}
	out.WriteByte(';')
	return out.String(), nil
}

func buildViaCLI(ctx context.Context, cli sqlitex.CLI, r recipe.Recipe, path string) ([]recipe.FileHashes, error) {
	if err := recipe.RemoveDatabase(path); err != nil {
		return nil, err
	}
	plan, err := r.Plan()
	if err != nil {
		return nil, err
	}
	opts := r.SQLiteOptions()
	var hashes []recipe.FileHashes
	for k, step := range plan {
		var sqlText strings.Builder
		fmt.Fprintf(&sqlText, "PRAGMA busy_timeout=%d;\n", opts.Conn.BusyTimeout.Milliseconds())
		if k == 0 {
			fmt.Fprintf(&sqlText, "PRAGMA page_size=%d;\n", opts.File.PageSize)
			fmt.Fprintf(&sqlText, "PRAGMA auto_vacuum=%s;\n", opts.File.AutoVacuum)
			fmt.Fprintf(&sqlText, "PRAGMA encoding='%s';\n", opts.File.Encoding)
			fmt.Fprintf(&sqlText, "PRAGMA application_id=%d;\n", opts.File.ApplicationID)
			fmt.Fprintf(&sqlText, "PRAGMA user_version=%d;\n", opts.File.UserVersion)
		}
		fmt.Fprintf(&sqlText, "PRAGMA journal_mode=%s;\n", opts.File.JournalMode)
		fmt.Fprintf(&sqlText, "PRAGMA synchronous=%s;\n", opts.Conn.Synchronous)
		sqlText.WriteString("PRAGMA foreign_keys=ON;\n")
		sqlText.WriteString("PRAGMA secure_delete=ON;\n")
		fmt.Fprintf(&sqlText, "PRAGMA cache_size=%d;\n", opts.Conn.CacheSize)
		fmt.Fprintf(&sqlText, "PRAGMA wal_autocheckpoint=%d;\n", opts.Conn.WALAutocheckpoint)
		sqlText.WriteString("BEGIN IMMEDIATE;\n")
		for _, stmt := range step.Statements {
			rendered, err := renderSQL(stmt)
			if err != nil {
				return nil, err
			}
			sqlText.WriteString(rendered)
			sqlText.WriteByte('\n')
		}
		sqlText.WriteString("COMMIT;\n")
		if _, err := cli.Exec(ctx, path, sqlText.String()); err != nil {
			return nil, fmt.Errorf("CLI step %d: %w", k, err)
		}
		if err := recipe.CheckAtRest(path); err != nil {
			return nil, fmt.Errorf("CLI step %d at rest: %w", k, err)
		}
		h, err := recipe.HashFile(path)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}

func main() {
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch01", "recipe-determinism")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	walRecipe := recipe.Default()
	delRecipe := walRecipe.WithJournalMode(sqlitex.JournalDelete)

	// Compute canonical hashes into scratch (avoid touching the repo's shared cache).
	canonWALDir := filepath.Join(scratch, "canon-wal")
	canonDelDir := filepath.Join(scratch, "canon-del")
	if err := os.MkdirAll(canonWALDir, 0o755); err != nil {
		fatal("mkdir canon-wal", err)
	}
	if err := os.MkdirAll(canonDelDir, 0o755); err != nil {
		fatal("mkdir canon-del", err)
	}
	canonWAL, err := walRecipe.ComputeStepHashes(ctx, canonWALDir)
	if err != nil {
		fatal("canon WAL", err)
	}
	canonDel, err := delRecipe.ComputeStepHashes(ctx, canonDelDir)
	if err != nil {
		fatal("canon DELETE", err)
	}
	wantWAL := canonWAL[walRecipe.Steps]
	wantDel := canonDel[delRecipe.Steps]

	// DET-01: 50 independent runs of the full 30-step WAL recipe across varied
	// directories, filenames (spaces, unicode, deep nesting, symlink, relative cwd),
	// and concurrent CPU + disk I/O load.
	runDET01(ctx, scratch, walRecipe, canonWAL)

	// DET-02: Extreme cache_size values in both WAL and rollback (delete) modes.
	runDET02(ctx, scratch, walRecipe, delRecipe, wantWAL, wantDel)

	// DET-03a/b/c: Other connection pragmas, locking_mode=EXCLUSIVE, and truncate/persist modes.
	runDET03(ctx, scratch, walRecipe, delRecipe, wantWAL, wantDel)

	// DET-04: Go driver vs C sqlite3 CLI building the full 30-step recipe (WAL and DELETE).
	runDET04(ctx, scratch, walRecipe, delRecipe, canonWAL, canonDel)

	// DET-05a/b/c/d: Stress-test Opus's Finding 2 (WAL one-connection batching across all 30 steps).
	runDET05(ctx, scratch, walRecipe)

	// DET-06: Stress-test Opus's Finding 1 (does WAL change counter advance across sessions that do NOT dirty page 1?).
	runDET06(ctx, scratch)

	// DET-07: Concurrent reader pinning WAL checkpoints across steps 5..25.
	runDET07(ctx, scratch, walRecipe)

	// DET-08a..e: Additional sources of variation missed by OUTLINE §4.2 and build-notes.
	runDET08(ctx, scratch, walRecipe, wantWAL)
}

func runDET01(ctx context.Context, scratch string, r recipe.Recipe, canon []recipe.FileHashes) {
	baseDir := filepath.Join(scratch, "det01-50runs")
	loadDir := filepath.Join(scratch, "det01-load")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		fatal("mkdir det01", err)
	}
	if err := os.MkdirAll(loadDir, 0o755); err != nil {
		fatal("mkdir load", err)
	}

	// Start background CPU + disk I/O load while the 50 builds run.
	var stopLoad atomic.Bool
	var loadWG sync.WaitGroup
	for w := range 2 {
		loadWG.Add(1)
		go func(id int) {
			defer loadWG.Done()
			buf := make([]byte, 64*1024)
			p := filepath.Join(loadDir, fmt.Sprintf("stress-%d.bin", id))
			var x uint64 = uint64(id + 1)
			for !stopLoad.Load() {
				for i := range 2000 {
					x = x*6364136223846793005 + uint64(i)
				}
				buf[0] = byte(x)
				_ = os.WriteFile(p, buf, 0o644)
			}
			_ = os.Remove(p)
		}(w)
	}

	// Create a symlinked directory target for symlink path testing.
	realTarget := filepath.Join(baseDir, "sym-target")
	symLink := filepath.Join(baseDir, "sym-link")
	if err := os.MkdirAll(realTarget, 0o755); err != nil {
		fatal("mkdir sym-target", err)
	}
	if err := os.Symlink(realTarget, symLink); err != nil {
		fatal("symlink", err)
	}

	// First 2 runs test relative paths from different working directories sequentially.
	origWD, err := os.Getwd()
	if err != nil {
		fatal("getwd", err)
	}
	relDirs := []string{
		filepath.Join(baseDir, "cwd-a"),
		filepath.Join(baseDir, "cwd with spaces", "nested"),
	}
	var mismatches atomic.Int32
	var stepMismatches atomic.Int32
	for i, d := range relDirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fatal("mkdir rel", err)
		}
		if err := os.Chdir(d); err != nil {
			fatal("chdir", err)
		}
		relName := fmt.Sprintf("rel-%d.db", i)
		var got []recipe.FileHashes
		err := r.BuildEach(ctx, relName, r.Steps, func(k int) error {
			h, err := recipe.HashFile(relName)
			if err != nil {
				return err
			}
			got = append(got, h)
			return nil
		})
		_ = os.Chdir(origWD)
		if err != nil {
			fatal("DET-01 rel build", err)
		}
		if got[r.Steps] != canon[r.Steps] {
			mismatches.Add(1)
		}
		if !slices.Equal(got, canon) {
			stepMismatches.Add(1)
		}
	}

	// Remaining 48 runs run across 4 concurrent workers with varied directory shapes & names.
	started := time.Now()
	jobs := make(chan int, totalFiftyRuns)
	for i := 2; i < totalFiftyRuns; i++ {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				var dbPath string
				switch idx % 5 {
				case 0:
					dbPath = filepath.Join(baseDir, fmt.Sprintf("r%02d.db", idx))
				case 1:
					dbPath = filepath.Join(baseDir, fmt.Sprintf("dir with spaces %02d", idx), fmt.Sprintf("db name %02d.sqlite", idx))
				case 2:
					dbPath = filepath.Join(baseDir, "a", "b", "c", "d", fmt.Sprintf("deep-%02d.db", idx))
				case 3:
					dbPath = filepath.Join(symLink, fmt.Sprintf("via-sym-%02d.db", idx))
				case 4:
					dbPath = filepath.Join(baseDir, fmt.Sprintf("café-%02d", idx), fmt.Sprintf("résumé-%02d.db", idx))
				}
				if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				var got []recipe.FileHashes
				err := r.BuildEach(ctx, dbPath, r.Steps, func(k int) error {
					h, err := recipe.HashFile(dbPath)
					if err != nil {
						return err
					}
					got = append(got, h)
					return nil
				})
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				if got[r.Steps] != canon[r.Steps] {
					mismatches.Add(1)
				}
				if !slices.Equal(got, canon) {
					stepMismatches.Add(1)
				}
				// Remove completed DBs to keep disk usage small during 50 runs.
				_ = recipe.RemoveDatabase(dbPath)
			}
		}()
	}
	wg.Wait()
	stopLoad.Store(true)
	loadWG.Wait()
	if firstErr != nil {
		fatal("DET-01 concurrent build", firstErr)
	}
	held := mismatches.Load() == 0 && stepMismatches.Load() == 0
	report(
		"DET-01",
		fmt.Sprintf("%d full 30-step WAL builds across varied cwds/paths/spaces/unicode/symlinks under CPU+IO load", totalFiftyRuns),
		fmt.Sprintf("all %d runs match canonical sha256 %s at every step 0..30", totalFiftyRuns, short(canon[r.Steps].Exact)),
		fmt.Sprintf("finalMismatches=%d stepMismatches=%d elapsed=%s", mismatches.Load(), stepMismatches.Load(), time.Since(started).Round(time.Millisecond)),
		held,
	)
}

func runDET02(ctx context.Context, scratch string, walR, delR recipe.Recipe, wantWAL, wantDel recipe.FileHashes) {
	cacheSizes := []int{1, 2, 5, 10, 50, 0, -10, -100, -2000, -64000}
	dir := filepath.Join(scratch, "det02-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det02", err)
	}
	var walDiffs, delDiffs []string
	for _, cs := range cacheSizes {
		pW := filepath.Join(dir, fmt.Sprintf("wal-cs%d.db", cs))
		hW, err := buildWithCustomSessionPragmas(ctx, walR, pW, func(o *sqlitex.Options) { o.Conn.CacheSize = cs }, nil)
		if err != nil {
			fatal(fmt.Sprintf("DET-02 wal cs=%d", cs), err)
		}
		if hW != wantWAL {
			walDiffs = append(walDiffs, fmt.Sprintf("cs=%d(exact=%v,masked=%v)", cs, hW.Exact == wantWAL.Exact, hW.Masked == wantWAL.Masked))
		}
		_ = recipe.RemoveDatabase(pW)

		pD := filepath.Join(dir, fmt.Sprintf("del-cs%d.db", cs))
		hD, err := buildWithCustomSessionPragmas(ctx, delR, pD, func(o *sqlitex.Options) { o.Conn.CacheSize = cs }, nil)
		if err != nil {
			fatal(fmt.Sprintf("DET-02 del cs=%d", cs), err)
		}
		if hD != wantDel {
			delDiffs = append(delDiffs, fmt.Sprintf("cs=%d(exact=%v,masked=%v)", cs, hD.Exact == wantDel.Exact, hD.Masked == wantDel.Masked))
		}
		_ = recipe.RemoveDatabase(pD)
	}
	held := len(walDiffs) == 0 && len(delDiffs) == 0
	report(
		"DET-02",
		fmt.Sprintf("full 30-step recipe across 10 cache_size values %v in both WAL and DELETE modes", cacheSizes),
		"every cache_size yields exact canonical sha256 in both modes",
		fmt.Sprintf("walDiffs=%v delDiffs=%v", walDiffs, delDiffs),
		held,
	)
}

func runDET03(ctx context.Context, scratch string, walR, delR recipe.Recipe, wantWAL, wantDel recipe.FileHashes) {
	dir := filepath.Join(scratch, "det03-pragmas")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det03", err)
	}
	cases := []struct {
		name  string
		mod   func(*sqlitex.Options)
		extra []string
	}{
		{"sync=OFF", func(o *sqlitex.Options) { o.Conn.Synchronous = sqlitex.SyncOff }, nil},
		{"sync=NORMAL", func(o *sqlitex.Options) { o.Conn.Synchronous = sqlitex.SyncNormal }, nil},
		{"sync=EXTRA", func(o *sqlitex.Options) { o.Conn.Synchronous = sqlitex.SyncExtra }, nil},
		{"autockpt=0", func(o *sqlitex.Options) { o.Conn.WALAutocheckpoint = 0 }, nil},
		{"autockpt=1", func(o *sqlitex.Options) { o.Conn.WALAutocheckpoint = 1 }, nil},
		{"autockpt=5", func(o *sqlitex.Options) { o.Conn.WALAutocheckpoint = 5 }, nil},
		{"mmap=65536", nil, []string{"PRAGMA mmap_size=65536"}},
		{"mmap=256MiB", nil, []string{"PRAGMA mmap_size=268435456"}},
		{"temp_store=FILE", nil, []string{"PRAGMA temp_store=1"}},
		{"temp_store=MEMORY", nil, []string{"PRAGMA temp_store=2"}},
		{"threads=4", nil, []string{"PRAGMA threads=4"}},
	}
	var diffs []string
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name+".db")
		h, err := buildWithCustomSessionPragmas(ctx, walR, p, tc.mod, tc.extra)
		if err != nil {
			diffs = append(diffs, fmt.Sprintf("%s(err=%v)", tc.name, err))
			continue
		}
		if h != wantWAL {
			diffs = append(diffs, fmt.Sprintf("%s(exact=%v,masked=%v)", tc.name, h.Exact == wantWAL.Exact, h.Masked == wantWAL.Masked))
		}
		_ = recipe.RemoveDatabase(p)
	}
	report(
		"DET-03a",
		"full 30-step WAL recipe (secure_delete=ON) across synchronous/autockpt/mmap_size/temp_store/threads variations",
		"exact canonical sha256 across all 11 per-connection pragma variations",
		fmt.Sprintf("diffs=%v", diffs),
		len(diffs) == 0,
	)

	// DET-03b: locking_mode=EXCLUSIVE in WAL mode.
	pExcl := filepath.Join(dir, "wal-excl.db")
	hExcl, errExcl := buildWithCustomSessionPragmas(ctx, walR, pExcl, nil, []string{"PRAGMA locking_mode=EXCLUSIVE"})
	heldExcl := errExcl == nil && hExcl == wantWAL
	gotExcl := fmt.Sprintf("exact=%v masked=%v", hExcl.Exact == wantWAL.Exact, hExcl.Masked == wantWAL.Masked)
	if errExcl != nil {
		gotExcl = fmt.Sprintf("err=%v", errExcl)
	}
	report(
		"DET-03b",
		"full 30-step WAL recipe with PRAGMA locking_mode=EXCLUSIVE per session",
		"database closes cleanly at rest and matches canonical sha256",
		gotExcl,
		heldExcl,
	)

	// DET-03c: rollback journal variants (truncate, persist) vs delete.
	var jDiffs []string
	for _, jm := range []string{sqlitex.JournalTruncate, sqlitex.JournalPersist} {
		p := filepath.Join(dir, "jm-"+jm+".db")
		if err := recipe.RemoveDatabase(p); err != nil {
			fatal("remove", err)
		}
		opts := delR.SQLiteOptions()
		opts.File.JournalMode = jm
		plan, err := delR.Plan()
		if err != nil {
			fatal("plan", err)
		}
		for _, s := range plan {
			db, err := sqlitex.Open(ctx, p, opts)
			if err != nil {
				fatal("open "+jm, err)
			}
			if err := recipe.ApplyStep(ctx, db, s); err != nil {
				db.Close()
				fatal("apply "+jm, err)
			}
			if err := db.Close(); err != nil {
				fatal("close "+jm, err)
			}
		}
		h, err := recipe.HashFile(p)
		if err != nil {
			fatal("hash "+jm, err)
		}
		_, jErr := os.Stat(p + "-journal")
		journalLeft := jErr == nil
		if h != wantDel {
			jDiffs = append(jDiffs, fmt.Sprintf("%s(exact=%v,journalLeft=%v)", jm, h.Exact == wantDel.Exact, journalLeft))
		}
		_ = recipe.RemoveDatabase(p)
	}
	report(
		"DET-03c",
		"full 30-step recipe in journal_mode=truncate and journal_mode=persist vs journal_mode=delete",
		"database file bytes equal rollback delete canonical sha256",
		fmt.Sprintf("diffs=%v", jDiffs),
		len(jDiffs) == 0,
	)
}

func runDET04(ctx context.Context, scratch string, walR, delR recipe.Recipe, canonWAL, canonDel []recipe.FileHashes) {
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate CLI for DET-04", err)
	}
	dir := filepath.Join(scratch, "det04-cli-recipe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det04", err)
	}
	walPath := filepath.Join(dir, "cli-wal.db")
	cliWAL, err := buildViaCLI(ctx, cli, walR, walPath)
	if err != nil {
		fatal("DET-04 CLI WAL", err)
	}
	delPath := filepath.Join(dir, "cli-del.db")
	cliDel, err := buildViaCLI(ctx, cli, delR, delPath)
	if err != nil {
		fatal("DET-04 CLI DELETE", err)
	}
	walMatch := slices.Equal(cliWAL, canonWAL)
	delMatch := slices.Equal(cliDel, canonDel)
	walFirstDiff, delFirstDiff := -1, -1
	for i := range canonWAL {
		if cliWAL[i] != canonWAL[i] {
			walFirstDiff = i
			break
		}
	}
	for i := range canonDel {
		if cliDel[i] != canonDel[i] {
			delFirstDiff = i
			break
		}
	}
	report(
		"DET-04",
		"build full 30-step recipe (steps 0..30) via pinned C sqlite3 CLI in WAL and DELETE modes",
		"all 31 per-step sha256 hashes match Go driver (modernc) in both modes",
		fmt.Sprintf("walMatch=%v(firstDiffStep=%d) delMatch=%v(firstDiffStep=%d)", walMatch, walFirstDiff, delMatch, delFirstDiff),
		walMatch && delMatch,
	)
}

func runDET05(ctx context.Context, scratch string, walR recipe.Recipe) {
	dir := filepath.Join(scratch, "det05-batches")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det05", err)
	}
	pBase := filepath.Join(dir, "one-conn-31tx.db")
	hBase, err := buildOneConnection(ctx, walR, pBase, "ON", "per-step", 1000)
	if err != nil {
		fatal("DET-05 base", err)
	}

	// DET-05a: All 31 steps merged into 1 single transaction on one WAL connection (secure_delete=ON).
	pSingle := filepath.Join(dir, "one-conn-1tx.db")
	hSingle, err := buildOneConnection(ctx, walR, pSingle, "ON", "single-tx", 1000)
	if err != nil {
		fatal("DET-05a", err)
	}
	eqA, sumA := diffSummary(pBase, pSingle)
	report(
		"DET-05a",
		"full 30-step WAL recipe on 1 connection (secure_delete=ON): 1 merged transaction vs 31 transactions",
		"byte-identical files (per 1.3 Finding 2: WAL + one connection + secure_delete=ON)",
		fmt.Sprintf("%s (exact=%v masked=%v)", sumA, hSingle.Exact == hBase.Exact, hSingle.Masked == hBase.Masked),
		eqA,
	)

	// DET-05b: Autocommit per statement (~1,700 transactions) vs 31 transactions.
	pAuto := filepath.Join(dir, "one-conn-autocommit.db")
	hAuto, err := buildOneConnection(ctx, walR, pAuto, "ON", "autocommit", 1000)
	if err != nil {
		fatal("DET-05b", err)
	}
	eqB, sumB := diffSummary(pBase, pAuto)
	report(
		"DET-05b",
		"full 30-step WAL recipe on 1 connection (secure_delete=ON): autocommit per statement (~1700 tx) vs 31 tx",
		"byte-identical files (per 1.3 Finding 2)",
		fmt.Sprintf("%s (exact=%v masked=%v)", sumB, hAuto.Exact == hBase.Exact, hAuto.Masked == hBase.Masked),
		eqB,
	)

	// DET-05c: Autocommit per statement with wal_autocheckpoint=1 vs 31 transactions.
	pAutoCkpt := filepath.Join(dir, "one-conn-autocommit-ckpt1.db")
	hAutoCkpt, err := buildOneConnection(ctx, walR, pAutoCkpt, "ON", "autocommit", 1)
	if err != nil {
		fatal("DET-05c", err)
	}
	eqC, sumC := diffSummary(pBase, pAutoCkpt)
	report(
		"DET-05c",
		"full 30-step WAL recipe on 1 connection (secure_delete=ON, wal_autocheckpoint=1): autocommit vs 31 tx",
		"byte-identical files despite hundreds of mid-recipe WAL checkpoints",
		fmt.Sprintf("%s (exact=%v masked=%v)", sumC, hAutoCkpt.Exact == hBase.Exact, hAutoCkpt.Masked == hBase.Masked),
		eqC,
	)

	// DET-05d: Batching with secure_delete=FAST (1 tx vs 31 tx on 1 connection).
	pFast31 := filepath.Join(dir, "fast-31tx.db")
	pFast1 := filepath.Join(dir, "fast-1tx.db")
	if _, err := buildOneConnection(ctx, walR, pFast31, "FAST", "per-step", 1000); err != nil {
		fatal("DET-05d 31tx", err)
	}
	if _, err := buildOneConnection(ctx, walR, pFast1, "FAST", "single-tx", 1000); err != nil {
		fatal("DET-05d 1tx", err)
	}
	eqD, sumD := diffSummary(pFast31, pFast1)
	report(
		"DET-05d",
		"full 30-step WAL recipe on 1 connection with PRAGMA secure_delete=FAST: 1 tx vs 31 tx",
		"secure_delete=FAST matches ON and produces identical files across batchings",
		sumD,
		eqD,
	)
}

func runDET06(ctx context.Context, scratch string) {
	// Test Opus's Correction 1 / Finding 6: "In WAL mode the file change counter
	// counts connection sessions, not commits."
	// Here 5 sessions each insert a row on page 2 WITHOUT modifying page 1
	// (no user_version change, no schema change, no new page allocation).
	dir := filepath.Join(scratch, "det06-wal-sessions-no-p1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det06", err)
	}
	opts := sqlitex.DefaultOptions()
	opts.File.JournalMode = sqlitex.JournalWAL
	opts.Conn.SecureDelete = true
	opts.SingleConnection = true

	pFive := filepath.Join(dir, "five-sessions.db")
	pOne := filepath.Join(dir, "one-session.db")
	initBase := func(p string) {
		_ = recipe.RemoveDatabase(p)
		db, err := sqlitex.Open(ctx, p, opts)
		if err != nil {
			fatal("det06 init", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t VALUES(1, 'init')"); err != nil {
			fatal("det06 schema", err)
		}
		if err := db.Close(); err != nil {
			fatal("det06 close init", err)
		}
	}
	initBase(pFive)
	initBase(pOne)

	for i := 2; i <= 6; i++ {
		db, err := sqlitex.Open(ctx, pFive, opts)
		if err != nil {
			fatal("det06 open 5", err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", i, fmt.Sprintf("row-%d", i)); err != nil {
			fatal("det06 ins 5", err)
		}
		if err := db.Close(); err != nil {
			fatal("det06 close 5", err)
		}
	}

	db, err := sqlitex.Open(ctx, pOne, opts)
	if err != nil {
		fatal("det06 open 1", err)
	}
	for i := 2; i <= 6; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", i, fmt.Sprintf("row-%d", i)); err != nil {
			fatal("det06 ins 1", err)
		}
	}
	if err := db.Close(); err != nil {
		fatal("det06 close 1", err)
	}

	eq, sum := diffSummary(pFive, pOne)
	// Under the literal unqualified claim "in WAL mode the number of connection sessions
	// changes the file (change counter counts connection sessions)", 5 sessions vs 1 session
	// would differ at bytes 24-27 and 92-95.
	report(
		"DET-06",
		"WAL mode: 5 connection sessions vs 1 connection each inserting a row on page 2 WITHOUT dirtying page 1",
		"file change counter (bytes 24-27, 92-95) differs (per unqualified claim 'WAL change counter counts connection sessions')",
		sum,
		!eq,
	)
}

func runDET07(ctx context.Context, scratch string, walR recipe.Recipe) {
	dir := filepath.Join(scratch, "det07-pinned-reader")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det07", err)
	}
	plan, err := walR.Plan()
	if err != nil {
		fatal("plan", err)
	}
	for _, secDel := range []bool{true, false} {
		label := "ON"
		if !secDel {
			label = "OFF"
		}
		pRef := filepath.Join(dir, "ref-"+label+".db")
		pPin := filepath.Join(dir, "pinned-"+label+".db")
		hRef, err := buildOneConnection(ctx, walR, pRef, label, "per-step", 1)
		if err != nil {
			fatal("det07 ref", err)
		}
		_ = recipe.RemoveDatabase(pPin)
		wOpts := walR.SQLiteOptions()
		wOpts.Conn.SecureDelete = secDel
		wOpts.Conn.WALAutocheckpoint = 1
		wdb, err := sqlitex.Open(ctx, pPin, wOpts)
		if err != nil {
			fatal("det07 writer open", err)
		}
		rOpts := walR.SQLiteOptions()
		rOpts.ReadOnly = true
		rOpts.SingleConnection = true
		var rdb *sqlitex.DB
		var rtx interface {
			Rollback() error
		}
		for k, s := range plan {
			if err := recipe.ApplyStep(ctx, wdb, s); err != nil {
				fatal("det07 apply", err)
			}
			if k == 5 {
				rdb, err = sqlitex.Open(ctx, pPin, rOpts)
				if err != nil {
					fatal("det07 reader open", err)
				}
				tx, err := rdb.BeginTx(ctx, nil)
				if err != nil {
					fatal("det07 reader begin", err)
				}
				var cnt int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&cnt); err != nil {
					fatal("det07 reader select", err)
				}
				rtx = tx
			}
			if k == 25 && rtx != nil {
				_ = rtx.Rollback()
				_ = rdb.Close()
				rtx = nil
			}
		}
		if err := wdb.Close(); err != nil {
			fatal("det07 writer close", err)
		}
		if err := recipe.CheckAtRest(pPin); err != nil {
			fatal("det07 at rest", err)
		}
		hPin, err := recipe.HashFile(pPin)
		if err != nil {
			fatal("det07 hash", err)
		}
		eq, sum := diffSummary(pRef, pPin)
		id := "DET-07a"
		expect := "exact byte match despite reader blocking WAL checkpoints across steps 5..25"
		if !secDel {
			id = "DET-07b"
			expect = "exact byte match even with secure_delete=OFF under reader-pinned WAL"
		}
		report(
			id,
			fmt.Sprintf("WAL 1-conn 30-step build (secure_delete=%s, autockpt=1) with concurrent reader pinning WAL steps 5..25", label),
			expect,
			fmt.Sprintf("%s (exact=%v masked=%v)", sum, hPin.Exact == hRef.Exact, hPin.Masked == hRef.Masked),
			eq,
		)
	}
}

func runDET08(ctx context.Context, scratch string, walR recipe.Recipe, wantWAL recipe.FileHashes) {
	dir := filepath.Join(scratch, "det08-missed-sources")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir det08", err)
	}

	// DET-08a: PRAGMA secure_delete=FAST vs ON on the canonical 30-step recipe.
	pFast := filepath.Join(dir, "canonical-fast.db")
	hFast, err := buildWithCustomSessionPragmas(ctx, walR, pFast, nil, []string{"PRAGMA secure_delete=FAST"})
	if err != nil {
		fatal("DET-08a", err)
	}
	report(
		"DET-08a",
		"30-step WAL recipe with PRAGMA secure_delete=FAST vs canonical secure_delete=ON",
		"exact canonical sha256 (FAST equivalent to ON)",
		fmt.Sprintf("exact=%v masked=%v fast=%s canon=%s", hFast.Exact == wantWAL.Exact, hFast.Masked == wantWAL.Masked, short(hFast.Exact), short(wantWAL.Exact)),
		hFast == wantWAL,
	)

	// DET-08b: Aborted SAVEPOINT + ROLLBACK TO inside a committed transaction (secure_delete=ON).
	pNoSP := filepath.Join(dir, "savepoint-clean.db")
	pWithSP := filepath.Join(dir, "savepoint-aborted.db")
	for _, p := range []string{pNoSP, pWithSP} {
		if err := walR.Build(ctx, p, 6); err != nil {
			fatal("DET-08b base", err)
		}
	}
	applySP := func(p string, doAborted bool) {
		db, err := walR.Open(ctx, p)
		if err != nil {
			fatal("DET-08b open", err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fatal("DET-08b begin", err)
		}
		if doAborted {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT sp1"); err != nil {
				fatal("DET-08b sp", err)
			}
			blob := bytes.Repeat([]byte{0xAB}, 12000)
			for i := 0; i < 10; i++ {
				if _, err := tx.ExecContext(ctx, "INSERT INTO documents(id, account_id, title, body) VALUES(?, 1, 'tmp', ?)", 9000+i, blob); err != nil {
					fatal("DET-08b sp ins", err)
				}
			}
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO sp1; RELEASE sp1"); err != nil {
				fatal("DET-08b rollback sp", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO events(id, account_id, kind, amount, note) VALUES(900001, 1, 'fee', 42, 'committed')"); err != nil {
			fatal("DET-08b ins", err)
		}
		if err := tx.Commit(); err != nil {
			fatal("DET-08b commit", err)
		}
		if err := db.Close(); err != nil {
			fatal("DET-08b close", err)
		}
	}
	applySP(pNoSP, false)
	applySP(pWithSP, true)
	eqSP, sumSP := diffSummary(pNoSP, pWithSP)
	report(
		"DET-08b",
		"same committed statements with secure_delete=ON, but run 2 performs SAVEPOINT + 10x12KB inserts + ROLLBACK TO",
		"identical database files since aborted savepoint leaves same committed rows",
		sumSP,
		eqSP,
	)

	// DET-08c: ANALYZE determinism in Go driver (ENABLE_STAT4) twice, and Go vs C CLI.
	pAnGo1 := filepath.Join(dir, "analyze-go1.db")
	pAnGo2 := filepath.Join(dir, "analyze-go2.db")
	pAnCLI := filepath.Join(dir, "analyze-cli.db")
	for _, p := range []string{pAnGo1, pAnGo2, pAnCLI} {
		if err := walR.Build(ctx, p, 10); err != nil {
			fatal("DET-08c base", err)
		}
	}
	for _, p := range []string{pAnGo1, pAnGo2} {
		db, err := walR.Open(ctx, p)
		if err != nil {
			fatal("DET-08c open", err)
		}
		if _, err := db.ExecContext(ctx, "ANALYZE"); err != nil {
			fatal("DET-08c analyze", err)
		}
		if err := db.Close(); err != nil {
			fatal("DET-08c close", err)
		}
	}
	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("DET-08c CLI", err)
	}
	if _, err := cli.Exec(ctx, pAnCLI, "PRAGMA secure_delete=ON;\nANALYZE;\n"); err != nil {
		fatal("DET-08c CLI analyze", err)
	}
	eqGoGo, sumGoGo := diffSummary(pAnGo1, pAnGo2)
	eqGoCLI, sumGoCLI := diffSummary(pAnGo1, pAnCLI)
	report(
		"DET-08c",
		"ANALYZE on step-10 recipe DB: Go twice (ENABLE_STAT4) and Go vs pinned C CLI (no STAT4)",
		"Go run 1 == Go run 2 AND Go == pinned C CLI byte-for-byte",
		fmt.Sprintf("go1_vs_go2=%s; go_vs_cli=%s", sumGoGo, sumGoCLI),
		eqGoGo && eqGoCLI,
	)

	// DET-08d: sqlitex.DefaultOptions() has SingleConnection=false.
	// In WAL mode, a concurrent read cursor forces database/sql to open a 2nd pooled
	// SQLite connection, which bumps the WAL change counter non-deterministically.
	pPoolClean := filepath.Join(dir, "pool-clean.db")
	pPoolChurn := filepath.Join(dir, "pool-churn.db")
	optsPool := sqlitex.DefaultOptions()
	optsPool.Conn.SecureDelete = true
	// Note: intentionally keeping optsPool.SingleConnection == false (the default).
	plan10, err := walR.WithSteps(10).Plan()
	if err != nil {
		fatal("plan10", err)
	}
	for _, churn := range []bool{false, true} {
		p := pPoolClean
		if churn {
			p = pPoolChurn
		}
		_ = recipe.RemoveDatabase(p)
		db, err := sqlitex.Open(ctx, p, optsPool)
		if err != nil {
			fatal("DET-08d open", err)
		}
		for k, s := range plan10 {
			if churn && k == 4 {
				// Hold a reader Rows cursor on conn 1 while ApplyStep acquires conn 2.
				rows, err := db.QueryContext(ctx, "SELECT id FROM accounts")
				if err != nil {
					fatal("DET-08d query", err)
				}
				rows.Next()
				if err := recipe.ApplyStep(ctx, db, s); err != nil {
					rows.Close()
					fatal("DET-08d apply during read", err)
				}
				rows.Close()
				continue
			}
			if err := recipe.ApplyStep(ctx, db, s); err != nil {
				fatal("DET-08d apply", err)
			}
		}
		if err := db.Close(); err != nil {
			fatal("DET-08d close", err)
		}
	}
	eqPool, sumPool := diffSummary(pPoolClean, pPoolChurn)
	report(
		"DET-08d",
		"WAL recipe steps 0..10 using sqlitex.DefaultOptions() (SingleConnection=false) with vs without concurrent read forcing a 2nd pooled conn",
		"identical database files regardless of database/sql connection pool churn",
		sumPool,
		eqPool,
	)

	// DET-08e: TZ environment variable: verify canonical recipe is immune to TZ,
	// while SQL datetime(..., 'localtime') varies with TZ (a variation source not in §4.2).
	pTZUTC := filepath.Join(dir, "tz-utc.db")
	pTZNY := filepath.Join(dir, "tz-ny.db")
	for _, tc := range []struct {
		p  string
		tz string
	}{
		{pTZUTC, "UTC"},
		{pTZNY, "America/New_York"},
	} {
		if err := walR.Build(ctx, tc.p, 6); err != nil {
			fatal("DET-08e base", err)
		}
		cmd := exec.CommandContext(ctx, cli.Path, "-batch", "-bail", "-init", os.DevNull, tc.p,
			"PRAGMA secure_delete=ON; INSERT INTO events(id, account_id, kind, amount, note) VALUES(900099, 1, 'fee', 0, datetime('2026-01-01 12:00:00', 'localtime'));")
		cmd.Env = append(os.Environ(), "TZ="+tc.tz)
		if out, err := cmd.CombinedOutput(); err != nil {
			fatal("DET-08e cli "+tc.tz, fmt.Errorf("%w: %s", err, string(out)))
		}
	}
	eqTZ, sumTZ := diffSummary(pTZUTC, pTZNY)
	report(
		"DET-08e",
		"fixed timestamp literal with SQL 'localtime' modifier under TZ=UTC vs TZ=America/New_York",
		"fixed timestamp literal produces identical bytes across timezones (not listed in §4.2)",
		sumTZ,
		eqTZ,
	)
}
