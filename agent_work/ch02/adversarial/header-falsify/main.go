package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/dbfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
	_ "modernc.org/sqlite"
)

const childReadHeader = "child-read-header"

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

// readHeaderChild reads the first 100 bytes of dbPath from a child process so
// we never violate the outline §4.1 POSIX-lock hazard by calling os.Open in a
// process that holds an active SQLite connection to the same file.
func readHeaderChild(path string) ([]byte, error) {
	cmdArgs, err := trace.SelfCommand(childReadHeader, path)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(cmdArgs[0], cmdArgs[1:]...).Output()
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(raw) != dbfile.HeaderSize {
		return nil, fmt.Errorf("bad child header output: %q (%v)", out, err)
	}
	return raw, nil
}

func childMain(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	buf := make([]byte, dbfile.HeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(hex.EncodeToString(buf))
}

func queryText(ctx context.Context, db sqlitex.Querier, query string) (string, error) {
	var v any
	if err := db.QueryRowContext(ctx, query).Scan(&v); err != nil {
		return "", err
	}
	if b, ok := v.([]byte); ok {
		return string(b), nil
	}
	return fmt.Sprint(v), nil
}

func logicalHeader(ctx context.Context, db sqlitex.Querier) ([]byte, error) {
	var data []byte
	if err := db.QueryRowContext(ctx, "SELECT data FROM sqlite_dbpage WHERE pgno = 1").Scan(&data); err != nil {
		return nil, err
	}
	if len(data) < dbfile.HeaderSize {
		return nil, fmt.Errorf("short page 1: %d bytes", len(data))
	}
	return data[:dbfile.HeaderSize], nil
}

func parseDBInfo(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) > 0 {
			out[strings.TrimSpace(name)] = fields[0]
		}
	}
	return out
}

func openRaw(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	}
	uri, err := sqlitex.FileURI(path, q)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
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
		"internal/trace",
		"internal/recipe",
		"internal/dbfile",
		"internal/equal",
		"internal/capture",
		"ch02-file-format-and-pages/experiments",
	}
	for _, d := range dirs {
		if err := copyTree(filepath.Join(repoRoot, d), filepath.Join(scratchRepo, d)); err != nil {
			return err
		}
	}
	return nil
}

type runResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runGoPkg(ctx context.Context, dir, pkg string, extraEnv []string) (runResult, error) {
	cmd := exec.CommandContext(ctx, "go", "run", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := runResult{stdout: stdout.String(), stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.exitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return res, err
		}
	}
	return res, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == childReadHeader {
		childMain(os.Args[2])
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch02", "header-falsify")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate CLI", err)
	}

	// HDR-01: All 8 valid page sizes (512..65536, with 65536 stored as 1).
	{
		sizes := []int{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
		var mismatches []string
		for _, sz := range sizes {
			p := filepath.Join(scratch, fmt.Sprintf("ps-%d.db", sz))
			opts := sqlitex.DefaultOptions()
			opts.File.PageSize = sz
			opts.SingleConnection = true
			db, err := sqlitex.Open(ctx, p, opts)
			if err != nil {
				fatal("open ps", err)
			}
			if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1,'a');"); err != nil {
				fatal("exec ps", err)
			}
			psPragma, err := queryText(ctx, db, "PRAGMA page_size")
			if err != nil {
				fatal("pragma page_size", err)
			}
			if err := db.Close(); err != nil {
				fatal("close ps", err)
			}
			f, err := dbfile.Open(p)
			if err != nil {
				mismatches = append(mismatches, fmt.Sprintf("%d: open err %v", sz, err))
				continue
			}
			stored := int(binary.BigEndian.Uint16(f.Header.Raw[16:18]))
			gotPS := f.Header.PageSize
			f.Close()

			out, err := cli.Exec(ctx, p, ".dbinfo\n")
			if err != nil {
				fatal("cli dbinfo", err)
			}
			cliPS := parseDBInfo(out)["database page size"]
			wantStored := sz
			if sz == 65536 {
				wantStored = 1
			}
			if gotPS != sz || stored != wantStored || psPragma != fmt.Sprint(sz) || cliPS != fmt.Sprint(sz) {
				mismatches = append(mismatches, fmt.Sprintf("%d(stored=%d,dbfile=%d,pragma=%s,cli=%s)", sz, stored, gotPS, psPragma, cliPS))
			}
		}
		report("HDR-01",
			"decode page size across all 8 valid sizes (512..65536, 65536 stored as 1) vs PRAGMA and CLI .dbinfo",
			"dbfile, stored bytes 16-17, PRAGMA page_size, and CLI .dbinfo agree for all 8 sizes",
			fmt.Sprintf("checked 8 sizes, mismatches=%d (%v)", len(mismatches), mismatches),
			len(mismatches) == 0)
	}

	// HDR-02: Truncated files at every boundary (0, 1, 16, 99, 100, 4095, 4097, 6144 bytes).
	{
		base := filepath.Join(scratch, "trunc-base.db")
		opts := sqlitex.DefaultOptions()
		opts.File.JournalMode = sqlitex.JournalDelete
		opts.SingleConnection = true
		db, err := sqlitex.Open(ctx, base, opts)
		if err != nil {
			fatal("open trunc base", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB); INSERT INTO t VALUES(1, zeroblob(5000)); INSERT INTO t VALUES(2, zeroblob(5000)); DELETE FROM t WHERE id=2;"); err != nil {
			fatal("seed trunc base", err)
		}
		if err := db.Close(); err != nil {
			fatal("close trunc base", err)
		}
		raw, err := os.ReadFile(base)
		if err != nil {
			fatal("read trunc base", err)
		}
		cuts := []int{0, 1, 16, 99, 100, 4095, 4097, len(raw) - 2048}
		var bad []string
		for _, n := range cuts {
			if n > len(raw) {
				continue
			}
			var panicked bool
			var parseErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicked = true
					}
				}()
				_, parseErr = dbfile.FromBytes(raw[:n])
			}()
			if panicked || !errors.Is(parseErr, dbfile.ErrTruncated) {
				bad = append(bad, fmt.Sprintf("len=%d(panic=%v,err=%v)", n, panicked, parseErr))
			}
		}
		// Compare SQLite's verdict on partial last freelist page vs partial live table page.
		halfFreePath := filepath.Join(scratch, "half-free.db")
		if err := os.WriteFile(halfFreePath, raw[:len(raw)-2048], 0o644); err != nil {
			fatal("write half-free", err)
		}
		roFree, err := openRaw(halfFreePath, true)
		if err != nil {
			fatal("open half-free", err)
		}
		freeIC, freeErr := queryText(ctx, roFree, "PRAGMA integrity_check")
		roFree.Close()

		halfLivePath := filepath.Join(scratch, "half-live.db")
		if err := os.WriteFile(halfLivePath, raw[:6144], 0o644); err != nil {
			fatal("write half-live", err)
		}
		roLive, err := openRaw(halfLivePath, true)
		if err != nil {
			fatal("open half-live", err)
		}
		liveIC, liveErr := queryText(ctx, roLive, "PRAGMA integrity_check")
		roLive.Close()

		held := len(bad) == 0 && freeErr == nil && freeIC == "ok" && (liveErr != nil || liveIC != "ok")
		report("HDR-02",
			"truncate DB at 0,1,16,99,100,4095,4097,half-last-page; compare dbfile vs SQLite",
			"dbfile returns ErrTruncated (0 panics) on all cuts; SQLite accepts half freelist page but rejects half live page",
			fmt.Sprintf("dbfile_bad=%v, sqlite_half_freelist=%q(err=%v), sqlite_half_live=%q(err=%v)", bad, freeIC, freeErr, liveIC, liveErr),
			held)
	}

	// HDR-03: WAL mode, ONE connection, 3 page-1 commits WITH cache eviction
	// (PRAGMA shrink_memory and cache_size=2 + scan) between commits.
	// Attacks 2.1 C1's claim ("WAL, one connection: every commit writes the same
	// change counter base+1, it does not advance per commit").
	{
		base := filepath.Join(scratch, "wal-evict-base.db")
		r := recipe.Default()
		if err := r.Build(ctx, base, 6); err != nil {
			fatal("build wal base", err)
		}
		baseHdr, err := readHeaderChild(base)
		if err != nil {
			fatal("read base hdr", err)
		}
		c0 := binary.BigEndian.Uint32(baseHdr[24:28])

		runEvict := func(name string, evict func(*sqlitex.DB) error) ([]uint32, uint32) {
			p := filepath.Join(scratch, name)
			if err := copyFile(base, p); err != nil {
				fatal("copy wal evict", err)
			}
			db, err := r.Open(ctx, p)
			if err != nil {
				fatal("open wal evict", err)
			}
			var seen []uint32
			for step := 7; step <= 9; step++ {
				if err := r.ApplyStep(ctx, db, step); err != nil {
					fatal("apply step", err)
				}
				lh, err := logicalHeader(ctx, db)
				if err != nil {
					fatal("logical hdr", err)
				}
				seen = append(seen, binary.BigEndian.Uint32(lh[24:28]))
				if err := evict(db); err != nil {
					fatal("evict", err)
				}
			}
			if err := db.Close(); err != nil {
				fatal("close wal evict", err)
			}
			finalHdr, err := readHeaderChild(p)
			if err != nil {
				fatal("read final hdr", err)
			}
			return seen, binary.BigEndian.Uint32(finalHdr[24:28])
		}

		shrinkCounters, shrinkFinal := runEvict("wal-shrink.db", func(db *sqlitex.DB) error {
			_, err := db.ExecContext(ctx, "PRAGMA shrink_memory")
			return err
		})
		cacheCounters, cacheFinal := runEvict("wal-cache2.db", func(db *sqlitex.DB) error {
			if _, err := db.ExecContext(ctx, "PRAGMA cache_size = 2"); err != nil {
				return err
			}
			// Scan documents (overflow pages) to evict page 1 from the 2-page pcache.
			var n int
			return db.QueryRowContext(ctx, "SELECT sum(length(body)) FROM documents").Scan(&n)
		})

		// 2.1 Part C1 + C4 rule: WAL commits that dirty page 1 write dbFileVers + 1,
		// where dbFileVers refreshes whenever page 1 is read into the page cache (readDbPage).
		// PRAGMA shrink_memory evicts page 1 so the counter advances each commit; when page 1
		// stays cached (as under the cache_size=2 scan here), the counter stays at base + 1.
		shrinkAdvanced := shrinkCounters[0] == c0+1 && shrinkCounters[1] == c0+2 && shrinkCounters[2] == c0+3 && shrinkFinal == c0+3
		cachedFlat := cacheCounters[0] == c0+1 && cacheCounters[1] == c0+1 && cacheCounters[2] == c0+1 && cacheFinal == c0+1
		report("HDR-03",
			"WAL mode on ONE connection: 3 page-1 commits separated by PRAGMA shrink_memory or cache_size=2 scan",
			fmt.Sprintf("2.1 C1+C4 cache rule: shrink_memory advances [%d %d %d] (final=%d); cached stays [%d %d %d] (final=%d)", c0+1, c0+2, c0+3, c0+3, c0+1, c0+1, c0+1, c0+1),
			fmt.Sprintf("base=%d, shrink_memory=%v (final=%d), cache_size=2=%v (final=%d)", c0, shrinkCounters, shrinkFinal, cacheCounters, cacheFinal),
			shrinkAdvanced && cachedFlat)
	}

	// HDR-04: WAL mode on ONE connection with explicit PRAGMA wal_checkpoint(TRUNCATE) between commits.
	{
		p := filepath.Join(scratch, "wal-ckpt-same-conn.db")
		r := recipe.Default()
		if err := r.Build(ctx, p, 6); err != nil {
			fatal("build wal ckpt", err)
		}
		baseHdr, err := readHeaderChild(p)
		if err != nil {
			fatal("read base hdr", err)
		}
		c0 := binary.BigEndian.Uint32(baseHdr[24:28])

		db, err := r.Open(ctx, p)
		if err != nil {
			fatal("open wal ckpt", err)
		}
		// Part 1: 2 non-page-1 updates with TRUNCATE checkpoint in between.
		for i := 0; i < 2; i++ {
			if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1"); err != nil {
				fatal("update", err)
			}
			var busy, logF, ckptF int
			if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logF, &ckptF); err != nil {
				fatal("ckpt", err)
			}
		}
		hdrAfterNonPage1, err := readHeaderChild(p)
		if err != nil {
			fatal("read hdr after non-p1", err)
		}
		nonPage1Unchanged := bytes.Equal(hdrAfterNonPage1, baseHdr)

		// Part 2: 2 page-1 commits with TRUNCATE checkpoint between them on the same connection.
		var cPage1 []uint32
		for step := 7; step <= 8; step++ {
			if err := r.ApplyStep(ctx, db, step); err != nil {
				fatal("apply", err)
			}
			var busy, logF, ckptF int
			if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logF, &ckptF); err != nil {
				fatal("ckpt2", err)
			}
			fh, err := readHeaderChild(p)
			if err != nil {
				fatal("read fh", err)
			}
			cPage1 = append(cPage1, binary.BigEndian.Uint32(fh[24:28]))
		}
		db.Close()
		report("HDR-04",
			"WAL one connection with PRAGMA wal_checkpoint(TRUNCATE) between non-page-1 and page-1 commits",
			fmt.Sprintf("non-page-1 updates leave file header byte-identical across checkpoints; page-1 commits stay at base+1 (%d) if pcache retains page 1", c0+1),
			fmt.Sprintf("non_page1_unchanged=%v, page1_after_ckpt_counters=%v (base=%d)", nonPage1Unchanged, cPage1, c0),
			nonPage1Unchanged && cPage1[0] == c0+1 && cPage1[1] == c0+1)
	}

	// HDR-05: Rollback mode non-mutating and rolled-back transactions must NOT advance bytes 24-27 / 92-95.
	{
		p := filepath.Join(scratch, "rb-noop.db")
		r := recipe.Default().WithJournalMode("delete")
		if err := r.Build(ctx, p, 3); err != nil {
			fatal("build rb noop", err)
		}
		before, err := readHeaderChild(p)
		if err != nil {
			fatal("read rb before", err)
		}
		c0 := binary.BigEndian.Uint32(before[24:28])
		db, err := r.Open(ctx, p)
		if err != nil {
			fatal("open rb noop", err)
		}
		// Read-only transaction
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin ro", err)
		}
		var cnt int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts").Scan(&cnt); err != nil {
			fatal("select", err)
		}
		if err := tx.Commit(); err != nil {
			fatal("commit ro", err)
		}
		afterRO, err := readHeaderChild(p)
		if err != nil {
			fatal("read afterRO", err)
		}
		// Zero-row write transaction
		if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE; UPDATE accounts SET balance = balance + 1 WHERE id = -999999; COMMIT;"); err != nil {
			fatal("zero-row write", err)
		}
		afterZeroWrite, err := readHeaderChild(p)
		if err != nil {
			fatal("read afterZeroWrite", err)
		}
		// Rolled-back write transaction
		if _, err := db.ExecContext(ctx, "BEGIN IMMEDIATE; UPDATE accounts SET balance = balance + 1 WHERE id = 1; ROLLBACK;"); err != nil {
			fatal("rollback write", err)
		}
		afterRollback, err := readHeaderChild(p)
		if err != nil {
			fatal("read afterRollback", err)
		}
		// Actual 1-row committed write
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1"); err != nil {
			fatal("real write", err)
		}
		afterReal, err := readHeaderChild(p)
		if err != nil {
			fatal("read afterReal", err)
		}
		db.Close()

		cRO := binary.BigEndian.Uint32(afterRO[24:28])
		cZW := binary.BigEndian.Uint32(afterZeroWrite[24:28])
		cRB := binary.BigEndian.Uint32(afterRollback[24:28])
		cReal := binary.BigEndian.Uint32(afterReal[24:28])
		vReal := binary.BigEndian.Uint32(afterReal[92:96])
		held := cRO == c0 && cZW == c0 && cRB == c0 && cReal == c0+1 && vReal == c0+1
		report("HDR-05",
			"rollback mode: read-only tx, 0-row UPDATE, rolled-back UPDATE, then real 1-row UPDATE",
			fmt.Sprintf("non-mutating/rolled-back txs keep counter=%d; real commit advances 24-27 and 92-95 to %d", c0, c0+1),
			fmt.Sprintf("base=%d, afterRO=%d, after0Row=%d, afterRollback=%d, afterReal=%d/%d", c0, cRO, cZW, cRB, cReal, vReal),
			held)
	}

	// HDR-06: Signed/negative header fields (user_version=-1, application_id=-1, default_cache_size=-100),
	// all 3 encodings, and all 3 auto-vacuum modes vs PRAGMA and CLI .dbinfo.
	{
		var diffs []string
		// Test negative signed fields
		negPath := filepath.Join(scratch, "neg-fields.db")
		db, err := openRaw(negPath, false)
		if err != nil {
			fatal("open neg", err)
		}
		for _, stmt := range []string{
			"PRAGMA user_version = -1",
			"PRAGMA application_id = -1",
			"PRAGMA default_cache_size = -100",
			"CREATE TABLE t(id INTEGER PRIMARY KEY)",
		} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				fatal(stmt, err)
			}
		}
		uvPragma, _ := queryText(ctx, db, "PRAGMA user_version")
		aidPragma, _ := queryText(ctx, db, "PRAGMA application_id")
		dcsPragma, _ := queryText(ctx, db, "PRAGMA default_cache_size")
		db.Close()

		f, err := dbfile.Open(negPath)
		if err != nil {
			fatal("parse neg", err)
		}
		h := f.Header
		f.Close()

		out, err := cli.Exec(ctx, negPath, ".dbinfo\n")
		if err != nil {
			fatal("cli dbinfo neg", err)
		}
		info := parseDBInfo(out)
		if fmt.Sprint(h.UserVersion) != uvPragma {
			diffs = append(diffs, fmt.Sprintf("user_version: dbfile=%d pragma=%s", h.UserVersion, uvPragma))
		}
		// CLI .dbinfo prints every 4-byte field unsigned (%u, src/shell.c.in:5561-5562),
		// whereas PRAGMA and dbfile decode user_version and application_id as signed int32.
		if fmt.Sprint(uint32(h.UserVersion)) != info["user version"] {
			diffs = append(diffs, fmt.Sprintf("user_version: uint32(dbfile)=%d dbinfo=%s", uint32(h.UserVersion), info["user version"]))
		}
		if fmt.Sprint(h.ApplicationID) != aidPragma {
			diffs = append(diffs, fmt.Sprintf("application_id: dbfile=%d pragma=%s", h.ApplicationID, aidPragma))
		}
		if fmt.Sprint(uint32(h.ApplicationID)) != info["application id"] {
			diffs = append(diffs, fmt.Sprintf("application_id: uint32(dbfile)=%d dbinfo=%s", uint32(h.ApplicationID), info["application id"]))
		}
		if fmt.Sprint(h.DefaultCacheSize) != dcsPragma {
			diffs = append(diffs, fmt.Sprintf("default_cache_size: dbfile=%d pragma=%s", h.DefaultCacheSize, dcsPragma))
		}
		if fmt.Sprint(h.DefaultCacheSize) != info["default cache size"] {
			diffs = append(diffs, fmt.Sprintf("default_cache_size: dbfile=%d dbinfo=%s", h.DefaultCacheSize, info["default cache size"]))
		}

		// Test encodings and auto_vacuum modes
		for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
			for _, av := range []string{sqlitex.AutoVacuumNone, sqlitex.AutoVacuumFull, sqlitex.AutoVacuumIncremental} {
				p := filepath.Join(scratch, fmt.Sprintf("enc-%s-av-%s.db", enc, av))
				opts := sqlitex.DefaultOptions()
				opts.File.Encoding = enc
				opts.File.AutoVacuum = av
				opts.SingleConnection = true
				d, err := sqlitex.Open(ctx, p, opts)
				if err != nil {
					fatal("open enc/av", err)
				}
				if _, err := d.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, x TEXT); INSERT INTO t VALUES(1,'héllo');"); err != nil {
					fatal("exec enc/av", err)
				}
				eP, _ := queryText(ctx, d, "PRAGMA encoding")
				avP, _ := queryText(ctx, d, "PRAGMA auto_vacuum")
				d.Close()

				df, err := dbfile.Open(p)
				if err != nil {
					diffs = append(diffs, fmt.Sprintf("%s/%s: open err %v", enc, av, err))
					continue
				}
				avMap := map[string]string{"0": "NONE", "1": "FULL", "2": "INCREMENTAL"}
				if df.Header.EncodingName() != eP {
					diffs = append(diffs, fmt.Sprintf("%s/%s: enc dbfile=%s pragma=%s", enc, av, df.Header.EncodingName(), eP))
				}
				if df.Header.AutoVacuumName() != avMap[avP] {
					diffs = append(diffs, fmt.Sprintf("%s/%s: av dbfile=%s pragma=%s", enc, av, df.Header.AutoVacuumName(), avMap[avP]))
				}
				df.Close()
			}
		}

		report("HDR-06",
			"negative user_version/application_id/default_cache_size (-1,-1,-100) + all 3 encodings x 3 auto_vacuum modes",
			"dbfile.Header agrees with signed PRAGMA, matches unsigned CLI .dbinfo (%u) bit-for-bit, and matches all 3x3 encoding/auto_vacuum modes",
			fmt.Sprintf("diffs=%d (%v)", len(diffs), diffs),
			len(diffs) == 0)
	}

	// HDR-07: Change-counter uint32 wrap-around (0xffffffff -> 0) in rollback mode.
	{
		p := filepath.Join(scratch, "wrap.db")
		opts := sqlitex.DefaultOptions()
		opts.File.JournalMode = sqlitex.JournalDelete
		opts.SingleConnection = true
		db, err := sqlitex.Open(ctx, p, opts)
		if err != nil {
			fatal("open wrap", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v INT); INSERT INTO t VALUES(1, 1);"); err != nil {
			fatal("seed wrap", err)
		}
		db.Close()

		raw, err := os.ReadFile(p)
		if err != nil {
			fatal("read wrap", err)
		}
		binary.BigEndian.PutUint32(raw[24:28], 0xffffffff)
		binary.BigEndian.PutUint32(raw[92:96], 0xffffffff)
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			fatal("write wrap", err)
		}

		db2, err := openRaw(p, false)
		if err != nil {
			fatal("reopen wrap", err)
		}
		if _, err := db2.ExecContext(ctx, "UPDATE t SET v = 2 WHERE id = 1"); err != nil {
			fatal("update wrap", err)
		}
		pcPragma, err := queryText(ctx, db2, "PRAGMA page_count")
		if err != nil {
			fatal("pragma pc wrap", err)
		}
		db2.Close()

		f, err := dbfile.Open(p)
		if err != nil {
			fatal("parse wrap", err)
		}
		h := f.Header
		pc := f.PageCount()
		src := f.PageCountSource()
		f.Close()

		held := h.ChangeCounter == 0 && h.VersionValidFor == 0 && h.InHeaderSizeValid() &&
			src == dbfile.PageCountFromHeader && fmt.Sprint(pc) == pcPragma
		report("HDR-07",
			"rollback commit when change counter and version-valid-for are 0xffffffff",
			"both wrap cleanly to 0, counters agree, in-header size remains valid and matches PRAGMA page_count",
			fmt.Sprintf("counter=%d, validFor=%d, inHdrValid=%v, src=%s, pc=%d, pragma=%s", h.ChangeCounter, h.VersionValidFor, h.InHeaderSizeValid(), src, pc, pcPragma),
			held)
	}

	// HDR-08: Header validation edge cases vs SQLite (write_version=3, read_version=2 with write_version=1, bad fields).
	{
		base := filepath.Join(scratch, "hdr-val-base.db")
		opts := sqlitex.DefaultOptions()
		opts.File.JournalMode = sqlitex.JournalDelete
		opts.SingleConnection = true
		db, err := sqlitex.Open(ctx, base, opts)
		if err != nil {
			fatal("open hdr val", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY); INSERT INTO t VALUES(1);"); err != nil {
			fatal("seed hdr val", err)
		}
		db.Close()
		good, err := os.ReadFile(base)
		if err != nil {
			fatal("read hdr val", err)
		}

		// Subcheck 1: invalid fields must be rejected with ErrNotADatabase and no panic.
		badMutations := []struct {
			name string
			edit func([]byte)
		}{
			{"pageSize=256", func(b []byte) { binary.BigEndian.PutUint16(b[16:18], 256) }},
			{"pageSize=3000", func(b []byte) { binary.BigEndian.PutUint16(b[16:18], 3000) }},
			{"pageSize=0", func(b []byte) { binary.BigEndian.PutUint16(b[16:18], 0) }},
			{"readVer=3", func(b []byte) { b[19] = 3 }},
			{"writeVer=0", func(b []byte) { b[18] = 0 }},
			{"fractions", func(b []byte) { b[21] = 63 }},
			{"usable<480", func(b []byte) { binary.BigEndian.PutUint16(b[16:18], 512); b[20] = 33 }},
			{"encoding=4", func(b []byte) { binary.BigEndian.PutUint32(b[56:60], 4) }},
			{"schemaFmt=5", func(b []byte) { binary.BigEndian.PutUint32(b[44:48], 5) }},
		}
		var invalidAccepted []string
		for _, m := range badMutations {
			cp := append([]byte(nil), good...)
			m.edit(cp)
			if _, err := dbfile.ParseHeader(cp); !errors.Is(err, dbfile.ErrNotADatabase) {
				invalidAccepted = append(invalidAccepted, fmt.Sprintf("%s(err=%v)", m.name, err))
			}
		}

		// Subcheck 2: write_version=1, read_version=2 (mismatched versions).
		// SQLite treats read_version=2 as WAL mode (lockBtree btree.c:3333),
		// whereas dbfile.Header.WALMode() checks ReadVersion==2 && WriteVersion==2.
		mismatch := append([]byte(nil), good...)
		mismatch[18] = 1
		mismatch[19] = 2
		mismatchPath := filepath.Join(scratch, "hdr-mismatch-ver.db")
		if err := os.WriteFile(mismatchPath, mismatch, 0o644); err != nil {
			fatal("write mismatch", err)
		}
		mh, mhErr := dbfile.ParseHeader(mismatch)
		mDB, err := openRaw(mismatchPath, false)
		if err != nil {
			fatal("open mismatch", err)
		}
		jm, jmErr := queryText(ctx, mDB, "PRAGMA journal_mode")
		mDB.Close()

		// Clean any side files left if SQLite switched to WAL.
		os.Remove(mismatchPath + "-wal")
		os.Remove(mismatchPath + "-shm")

		// If SQLite reports "wal" while dbfile reports WALMode()==false on a file dbfile accepted,
		// that's an agreement gap on mismatched bytes 18-19.
		walDisagrees := mhErr == nil && jmErr == nil && strings.EqualFold(jm, "wal") != mh.WALMode()
		report("HDR-08",
			"header validation on 9 malformed headers + mismatched write_version=1/read_version=2 vs SQLite",
			"all 9 malformed headers rejected with ErrNotADatabase, and WALMode() agrees with SQLite PRAGMA journal_mode",
			fmt.Sprintf("malformed_accepted=%v, w=1/r=2: dbfile_err=%v WALMode=%v sqlite_journal_mode=%q(err=%v)", invalidAccepted, mhErr, mh.WALMode(), jm, jmErr),
			len(invalidAccepted) == 0 && !walDisagrees)
	}

	// HDR-09: Pin enforcement "must be able to fail" across all 3 ch02 experiments.
	{
		scratchRepo := filepath.Join(scratch, "scratch-repo")
		if err := buildScratchRepo(repoRoot, scratchRepo); err != nil {
			fatal("build scratch repo", err)
		}
		pinPath := filepath.Join(scratchRepo, "sqlite", "PIN")
		pinBytes, err := os.ReadFile(pinPath)
		if err != nil {
			fatal("read scratch PIN", err)
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
			{"2.1", "./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes"},
			{"2.2", "./ch02-file-format-and-pages/experiments/2.2-page-census"},
			{"2.3", "./ch02-file-format-and-pages/experiments/2.3-page-manifest"},
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
		report("HDR-09",
			"mutate sqlite/PIN in scratch repo and run ch02 experiments 2.1, 2.2, 2.3",
			"all 3 experiments exit non-zero and report sqlite_source_id MISMATCH on line 1",
			fmt.Sprintf("results=[%s], failures=%v", strings.Join(summary, ", "), failed),
			len(failed) == 0)
	}

	// HDR-10: Experiment 2.1 with SQLITE3_BIN pointing to a wrong-version CLI ("must be able to fail").
	// OUTLINE §1 item 4: "Experiments that run the CLI check its id the same way [exit non-zero if not pinned]."
	{
		fakeCLI := filepath.Join(scratch, "fake-sqlite3.sh")
		script := "#!/usr/bin/env bash\necho '2099-01-01 00:00:00 deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef'\n"
		if err := os.WriteFile(fakeCLI, []byte(script), 0o755); err != nil {
			fatal("write fake CLI", err)
		}
		workDir := filepath.Join(scratch, "cli-mismatch-work")
		res, err := runGoPkg(ctx, repoRoot, "./ch02-file-format-and-pages/experiments/2.1-header-and-what-a-commit-changes",
			[]string{"SQLITE3_BIN=" + fakeCLI, "COURSE_WORK_DIR=" + workDir})
		if err != nil {
			fatal("run 2.1 fake CLI", err)
		}
		var line2 string
		lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
		if len(lines) >= 2 {
			line2 = lines[1]
		}
		report("HDR-10",
			"run experiment 2.1 with SQLITE3_BIN pointing to a wrong-version sqlite3 CLI",
			"2.1 exits non-zero when an explicit SQLITE3_BIN fails the pin check (OUTLINE §1 item 4)",
			fmt.Sprintf("exit=%d, line2=%q", res.exitCode, line2),
			res.exitCode != 0)
	}
}
