// Group 4 (r2-canonical-and-pin): Adversarial attacks on Chapter 3 Experiment
// 3.3 (same-rows-different-bytes, rung R2) and "must be able to fail" pin
// checks for all three ch03 experiments (3.1, 3.2, 3.3) plus 3.3's NO-DBHASH
// fallback.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
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

// --- Exact R2 dump / replay functions matching 3.3-same-rows-different-bytes/main.go ---

func rowidName(name string) string {
	switch strings.ToLower(name) {
	case "rowid", "_rowid_", "oid":
		return name
	}
	return equal.QuoteIdentifier(name)
}

func insertStatement(t equal.TableInfo, values []string) string {
	var names []string
	if !t.WithoutRowid {
		names = append(names, rowidName(t.RowidColumn))
	}
	for _, c := range t.Columns {
		names = append(names, equal.QuoteIdentifier(c))
	}
	return fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", equal.QuoteIdentifier(t.Name), strings.Join(names, ", "), strings.Join(values, ", "))
}

func sqlLiteral(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		switch {
		case math.IsInf(v, 1):
			return "1e999"
		case math.IsInf(v, -1):
			return "-1e999"
		}
		text := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(text, ".eEn") {
			text += ".0"
		}
		return text
	case string:
		parts := strings.Split(v, "\x00")
		for i, part := range parts {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "''") + "'"
		}
		return strings.Join(parts, "||char(0)||")
	case []byte:
		return "X'" + strings.ToUpper(hex.EncodeToString(v)) + "'"
	}
	fatal("sqlLiteral", fmt.Errorf("unsupported value %T", value))
	return ""
}

// r2Dump mirrors the repaired 3.3-same-rows-different-bytes/main.go's dump()
// verbatim so we can test its edge cases against arbitrary valid SQLite databases.
func r2Dump(ctx context.Context, path string) (string, error) {
	db, err := equal.OpenExisting(ctx, path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return "", err
	}
	defer conn.ExecContext(ctx, "COMMIT")
	var tables, indexes, others []string
	var body strings.Builder
	var sequence []string
	pragmas := map[string]string{}
	schema, err := conn.QueryContext(ctx, "SELECT type, name, sql FROM main.sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid")
	if err != nil {
		return "", err
	}
	for schema.Next() {
		var kind, name, statement string
		if err := schema.Scan(&kind, &name, &statement); err != nil {
			schema.Close()
			return "", err
		}
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		switch kind {
		case "table":
			tables = append(tables, statement+";")
		case "index":
			indexes = append(indexes, statement+";")
		default:
			others = append(others, statement+";")
		}
	}
	if err := schema.Err(); err != nil {
		schema.Close()
		return "", err
	}
	if err := schema.Close(); err != nil {
		return "", err
	}
	err = equal.WalkLogical(ctx, conn, equal.LogicalVisitor{
		Row: func(t equal.TableInfo, values []any) error {
			literals := make([]string, len(values))
			for i, v := range values {
				literals[i] = sqlLiteral(v)
			}
			line := insertStatement(t, literals) + ";\n"
			switch {
			case t.Name == "sqlite_sequence":
				sequence = append(sequence, line)
			case strings.HasPrefix(t.Name, "sqlite_"):
				return fmt.Errorf("table %s: this capture handles sqlite_sequence only", t.Name)
			default:
				body.WriteString(line)
			}
			return nil
		},
		Pragma: func(name, value string) error {
			pragmas[name] = value
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "PRAGMA page_size=%s;\nPRAGMA auto_vacuum=%s;\nPRAGMA encoding='%s';\n",
		pragmas["page_size"], strings.ToUpper(pragmas["auto_vacuum"]), pragmas["encoding"])
	if pragmas["journal_mode"] == "wal" {
		out.WriteString("PRAGMA journal_mode=WAL;\n")
	}
	out.WriteString("BEGIN;\n")
	for _, s := range tables {
		out.WriteString(s + "\n")
	}
	out.WriteString(body.String())
	if len(sequence) > 0 {
		out.WriteString("DELETE FROM sqlite_sequence;\n")
		for _, s := range sequence {
			out.WriteString(s)
		}
	}
	for _, s := range append(indexes, others...) {
		out.WriteString(s + "\n")
	}
	fmt.Fprintf(&out, "PRAGMA user_version=%s;\nPRAGMA application_id=%s;\nCOMMIT;\n",
		pragmas["user_version"], pragmas["application_id"])
	return out.String(), nil
}

func r2Replay(ctx context.Context, script, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		return err
	}
	uri, err := sqlitex.FileURI(path, url.Values{})
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, script); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}

func vacuumInto(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := recipe.RemoveDatabase(dst); err != nil {
		return err
	}
	db, err := equal.OpenExisting(ctx, src)
	if err != nil {
		return err
	}
	_, execErr := db.ExecContext(ctx, "VACUUM INTO ?", dst)
	closeErr := db.Close()
	if execErr != nil {
		return execErr
	}
	return closeErr
}

func buildDB(ctx context.Context, path string, opts sqlitex.Options, oneTx bool, stmts []string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir", err)
	}
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove", err)
	}
	opts.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, opts)
	if err != nil {
		fatal("open "+path, err)
	}
	if oneTx {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin", err)
		}
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				tx.Rollback()
				db.Close()
				fatal(s, err)
			}
		}
		if err := tx.Commit(); err != nil {
			db.Close()
			fatal("commit", err)
		}
	} else {
		for _, s := range stmts {
			if _, err := db.ExecContext(ctx, s); err != nil {
				db.Close()
				fatal(s, err)
			}
		}
	}
	if err := db.Close(); err != nil {
		fatal("close "+path, err)
	}
	return path
}

type cmdRes struct {
	stdout   string
	stderr   string
	exitCode int
}

func runGoPkg(ctx context.Context, dir, pkg string, extraEnv []string) (cmdRes, error) {
	cmd := exec.CommandContext(ctx, "go", "run", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return cmdRes{}, err
		}
	}
	return cmdRes{stdout: outBuf.String(), stderr: errBuf.String(), exitCode: code}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// buildScratchRepo clones the minimal repo layout needed to compile and run
// ch03 experiments in isolation with a modified sqlite/PIN (and no _tools/bin).
func buildScratchRepo(repoRoot, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, f), data, 0o644); err != nil {
			return err
		}
	}
	for _, d := range []string{"sqlite", "internal", filepath.Join("ch03-btrees-and-page-management", "experiments")} {
		if err := copyTree(filepath.Join(repoRoot, d), filepath.Join(dst, d)); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	ctx := context.Background()
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("RepoRoot", err)
	}
	scratch := filepath.Join(root, "_work", "adv-ch03", "r2-canonical-and-pin")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// R2-01: WAL batching claim (build-notes correction 1) under harder workloads:
	// (a) append-only with a scrambled secondary index, (b) append-only with tiny
	// cache_size=4 forcing mid-tx page-cache spills, (c) mixed INSERT+DELETE+re-INSERT.
	{
		dir := filepath.Join(scratch, "r201")
		var idxStmts []string
		idxStmts = append(idxStmts,
			"CREATE TABLE t(id INTEGER PRIMARY KEY, k TEXT, v BLOB)",
			"CREATE INDEX t_k ON t(k)")
		for i := 1; i <= 150; i++ {
			idxStmts = append(idxStmts, fmt.Sprintf("INSERT INTO t VALUES(%d, 'k%04d', zeroblob(400))", i, (i*47)%150))
		}

		var mixStmts []string
		mixStmts = append(mixStmts, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)")
		for i := 1; i <= 80; i++ {
			mixStmts = append(mixStmts, fmt.Sprintf("INSERT INTO t VALUES(%d, zeroblob(1000))", i))
		}
		mixStmts = append(mixStmts, "DELETE FROM t WHERE id BETWEEN 21 AND 60")
		for i := 101; i <= 130; i++ {
			mixStmts = append(mixStmts, fmt.Sprintf("INSERT INTO t VALUES(%d, zeroblob(1000))", i))
		}

		optsNorm := sqlitex.DefaultOptions()
		optsNorm.Conn.SecureDelete = true
		optsNorm.Conn.Synchronous = sqlitex.SyncOff

		optsTinyCache := optsNorm
		optsTinyCache.Conn.CacheSize = 4 // 4 pages -> spills heavily in 1-tx build

		runPair := func(name string, opts sqlitex.Options, stmts []string) bool {
			p1 := buildDB(ctx, filepath.Join(dir, name+"-onetx.db"), opts, true, stmts)
			p2 := buildDB(ctx, filepath.Join(dir, name+"-perstmt.db"), opts, false, stmts)
			res, err := equal.CompareFiles(p1, p2, 4096)
			if err != nil {
				fatal("CompareFiles "+name, err)
			}
			return res.Equal
		}

		eqIdx := runPair("indexed", optsNorm, idxStmts)
		eqSpill := runPair("spill", optsTinyCache, idxStmts)
		eqMix := runPair("mixed-del", optsNorm, mixStmts)

		held := eqIdx && eqSpill && eqMix
		report("R2-01",
			"WAL one-tx vs per-statement byte equality under (a) scrambled secondary index, (b) tiny cache_size=4 spills, (c) INSERT+DELETE+re-INSERT",
			"all three WAL pairs are byte-identical (E1 equal) regardless of transaction boundaries",
			fmt.Sprintf("indexed_E1=%v, tiny_cache_spill_E1=%v, insert_delete_reinsert_E1=%v", eqIdx, eqSpill, eqMix),
			held)
	}

	// R2-02: R2 dump() + replay() -> VACUUM INTO vs original VACUUM INTO when
	// original tables were created in non-alphabetical order (zebra before alpha).
	{
		dir := filepath.Join(scratch, "r202")
		opts := sqlitex.DefaultOptions()
		opts.Conn.SecureDelete = true
		orig := buildDB(ctx, filepath.Join(dir, "orig.db"), opts, true, []string{
			"CREATE TABLE zebra(id INTEGER PRIMARY KEY, v TEXT)",
			"CREATE TABLE alpha(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO zebra VALUES(1, 'z1'), (2, 'z2')",
			"INSERT INTO alpha VALUES(1, 'a1'), (2, 'a2')",
		})
		script, err := r2Dump(ctx, orig)
		if err != nil {
			fatal("r2Dump", err)
		}
		replayed := filepath.Join(dir, "replayed.db")
		if err := r2Replay(ctx, script, replayed); err != nil {
			fatal("r2Replay", err)
		}
		e3, _, _, err := equal.CompareLogicalFiles(ctx, orig, replayed)
		if err != nil {
			fatal("E3", err)
		}
		cOrig, cRep := filepath.Join(dir, "orig-vac.db"), filepath.Join(dir, "replayed-vac.db")
		if err := vacuumInto(ctx, orig, cOrig); err != nil {
			fatal("vacuum orig", err)
		}
		if err := vacuumInto(ctx, replayed, cRep); err != nil {
			fatal("vacuum replayed", err)
		}
		e1, err := equal.CompareFiles(cOrig, cRep, 4096)
		if err != nil {
			fatal("E1", err)
		}
		e2, err := equal.ComparePageFiles(cOrig, cRep, 4096)
		if err != nil {
			fatal("E2", err)
		}
		// Claim in 3.3 / R2 (repaired): dump preserves sqlite_schema rowid order, so the
		// replayed copy is E3-equal AND canonicalizes to the source's VACUUM INTO sha256 at E1.
		report("R2-02",
			"R2 dump + replay + VACUUM INTO on DB with tables created in non-alphabetical order (zebra before alpha)",
			"replayed DB is E3-equal to original AND its VACUUM INTO matches original's VACUUM INTO at E1",
			fmt.Sprintf("E3_equal=%v, VACUUM_INTO_E1_equal=%v(diffPages=%d), VACUUM_INTO_E2_equal=%v",
				e3.Equal, e1.Equal, e1.DifferingPages, e2.Equal),
			e3.Equal && e1.Equal)
	}

	// R2-03a: R2 dump() + replay() round-trip on rich valid SQLite features:
	// AUTOINCREMENT (sqlite_sequence), WITHOUT ROWID, DATETIME text, +Inf/-Inf,
	// quotes & newlines, empty blob vs NULL.
	{
		dir := filepath.Join(scratch, "r203a")
		opts := sqlitex.DefaultOptions()
		opts.Conn.SecureDelete = true
		orig := buildDB(ctx, filepath.Join(dir, "rich.db"), opts, true, []string{
			"CREATE TABLE seq_t(id INTEGER PRIMARY KEY AUTOINCREMENT, d DATETIME, f REAL, s TEXT, b BLOB)",
			"INSERT INTO seq_t(d, f, s, b) VALUES('2026-11-21 09:30:00', 1e999, 'it''s a\nline', x'')",
			"INSERT INTO seq_t(d, f, s, b) VALUES('2026-11-21 09:31:00', -1e999, 'keep', NULL)",
			"INSERT INTO seq_t(d, f, s, b) VALUES('2026-11-21 09:32:00', 1.0, 'delete-me', x'DEADBEEF')",
			"DELETE FROM seq_t WHERE id = 3",
			"CREATE TABLE wr_t(k TEXT PRIMARY KEY, v) WITHOUT ROWID",
			"INSERT INTO wr_t VALUES('b', 1), ('a', 1.0)",
			"PRAGMA user_version = 19",
		})
		script, err := r2Dump(ctx, orig)
		if err != nil {
			fatal("r2Dump rich", err)
		}
		rep := filepath.Join(dir, "rich-replayed.db")
		replayErr := r2Replay(ctx, script, rep)
		var e3 equal.LogicalResult
		if replayErr == nil {
			e3, _, _, err = equal.CompareLogicalFiles(ctx, orig, rep)
			if err != nil {
				fatal("E3 rich", err)
			}
		}
		report("R2-03a",
			"R2 dump + replay round-trip on AUTOINCREMENT (sqlite_sequence), WITHOUT ROWID, DATETIME, +-Inf, quotes/newlines, empty blob vs NULL",
			"replay succeeds and matches original at E3",
			fmt.Sprintf("replay_err=%v, E3=%s", replayErr, e3),
			replayErr == nil && e3.Equal)
	}

	// R2-03b: R2 dump() + replay() on TEXT containing an embedded NUL byte (char(0))
	// AND a table whose column name contains "?, " (attacking 3.3's sqlLiteral and strings.Replace).
	{
		dir := filepath.Join(scratch, "r203b")
		opts := sqlitex.DefaultOptions()

		// Subcase 1: embedded NUL byte in TEXT.
		pNul := buildDB(ctx, filepath.Join(dir, "nul.db"), opts, true, []string{
			"CREATE TABLE t(id INTEGER PRIMARY KEY, s TEXT)",
			"INSERT INTO t VALUES(1, 'head' || char(0) || 'tail')",
		})
		scriptNul, err := r2Dump(ctx, pNul)
		if err != nil {
			fatal("dump nul", err)
		}
		repNul := filepath.Join(dir, "nul-rep.db")
		errRepNul := r2Replay(ctx, scriptNul, repNul)
		nulE3OK := false
		nulDetail := ""
		if errRepNul != nil {
			nulDetail = "replay_err=" + errRepNul.Error()
		} else {
			e3, _, _, err := equal.CompareLogicalFiles(ctx, pNul, repNul)
			if err != nil {
				nulDetail = "compare_err=" + err.Error()
			} else {
				nulE3OK = e3.Equal
				nulDetail = e3.String()
			}
		}

		// Subcase 2: 1-column WITHOUT ROWID table whose column name is "?"
		// (triggers strings.Replace(..., "?", literal, 1) in 3.3 line 215 inside the identifier!).
		pQ := buildDB(ctx, filepath.Join(dir, "qmark.db"), opts, true, []string{
			`CREATE TABLE "t?"("?" TEXT PRIMARY KEY) WITHOUT ROWID`,
			`INSERT INTO "t?" VALUES('val')`,
		})
		scriptQ, err := r2Dump(ctx, pQ)
		if err != nil {
			fatal("dump qmark", err)
		}
		repQ := filepath.Join(dir, "qmark-rep.db")
		errRepQ := r2Replay(ctx, scriptQ, repQ)
		qE3OK := false
		qDetail := ""
		if errRepQ != nil {
			qDetail = "replay_err=" + errRepQ.Error()
		} else {
			e3, _, _, err := equal.CompareLogicalFiles(ctx, pQ, repQ)
			if err != nil {
				qDetail = "compare_err=" + err.Error()
			} else {
				qE3OK = e3.Equal
				qDetail = e3.String()
			}
		}

		report("R2-03b",
			`R2 dump + replay on (1) TEXT containing embedded NUL byte char(0), and (2) 1-col WITHOUT ROWID table "t?"("?" TEXT PRIMARY KEY)`,
			"both valid SQLite databases round-trip cleanly and match original at E3",
			fmt.Sprintf("embedded_NUL:(ok=%v %s); col_with_qmark:(ok=%v %s)", nulE3OK, nulDetail, qE3OK, qDetail),
			nulE3OK && qE3OK)
	}

	// R2-04: VACUUM INTO header preservation vs resets under non-default settings
	// (page_size=512, auto_vacuum=FULL, UTF-16le, user_version=77, app_id=999, default_cache_size=1234)
	// AND refusal of an existing non-empty target file (build-notes vacuum.c:143-427).
	{
		dir := filepath.Join(scratch, "r204")
		opts := sqlitex.DefaultOptions()
		opts.File.PageSize = 512
		opts.File.AutoVacuum = sqlitex.AutoVacuumFull
		opts.File.Encoding = "UTF-16le"
		opts.File.ApplicationID = 999
		opts.File.UserVersion = 77
		src := buildDB(ctx, filepath.Join(dir, "src.db"), opts, false, []string{
			"PRAGMA default_cache_size = 1234",
			"CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)",
			"INSERT INTO t VALUES(1, 'hello')",
		})
		dst := filepath.Join(dir, "canon.db")
		if err := vacuumInto(ctx, src, dst); err != nil {
			fatal("vacuum dst", err)
		}
		raw, err := os.ReadFile(dst)
		if err != nil {
			fatal("read canon", err)
		}
		ps := int(binary.BigEndian.Uint16(raw[16:18]))
		wv, rv := raw[18], raw[19]
		chg := binary.BigEndian.Uint32(raw[24:28])
		defCache := int32(binary.BigEndian.Uint32(raw[48:52]))
		enc := binary.BigEndian.Uint32(raw[56:60])
		uv := binary.BigEndian.Uint32(raw[60:64])
		appID := binary.BigEndian.Uint32(raw[68:72])
		av := binary.BigEndian.Uint32(raw[52:56])

		// Refusal of existing non-empty target file:
		db, err := equal.OpenExisting(ctx, src)
		if err != nil {
			fatal("open src", err)
		}
		_, existingErr := db.ExecContext(ctx, "VACUUM INTO ?", dst)
		db.Close()

		held := ps == 512 && wv == 1 && rv == 1 && chg == 1 && defCache == 1234 &&
			enc == 2 && uv == 77 && appID == 999 && av > 0 &&
			existingErr != nil && strings.Contains(existingErr.Error(), "already exists")
		report("R2-04",
			"VACUUM INTO on page_size=512, auto_vacuum=FULL, UTF-16le, user_version=77, app_id=999, default_cache_size=1234 + repeat into existing target",
			"preserves page_size/av/encoding/user_version/app_id/default_cache_size, resets journal_mode to rollback (1,1) and change_counter=1, refuses existing target",
			fmt.Sprintf("ps=%d ver=%d,%d chg=%d defCache=%d enc=%d uv=%d appID=%d avPtr=%d existingErr=%v",
				ps, wv, rv, chg, defCache, enc, uv, appID, av, existingErr),
			held)
	}

	// R2-05: "Must be able to fail" — Pin enforcement across all 3 ch03 experiments (3.1, 3.2, 3.3).
	scratchRepo := filepath.Join(scratch, "scratch-repo")
	if err := buildScratchRepo(root, scratchRepo); err != nil {
		fatal("buildScratchRepo", err)
	}
	pinPath := filepath.Join(scratchRepo, "sqlite", "PIN")
	origPIN, err := os.ReadFile(pinPath)
	if err != nil {
		fatal("read PIN", err)
	}
	{
		mutated := strings.ReplaceAll(string(origPIN),
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
			{"3.1", "./ch03-btrees-and-page-management/experiments/3.1-pages-per-insert"},
			{"3.2", "./ch03-btrees-and-page-management/experiments/3.2-delete-and-reuse"},
			{"3.3", "./ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes"},
		}
		var failed, summary []string
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
		report("R2-05",
			"mutate sqlite/PIN in isolated scratch repo and run ch03 experiments 3.1, 3.2, 3.3",
			"all 3 experiments exit non-zero and report pin mismatch on line 1",
			fmt.Sprintf("results=[%s], failures=%v", strings.Join(summary, ", "), failed),
			len(failed) == 0)
	}

	// R2-06: Experiment 3.3 graceful NO-DBHASH fallback when dbhash is absent.
	{
		// Restore valid PIN in scratchRepo (which has no _tools/bin/dbhash).
		if err := os.WriteFile(pinPath, origPIN, 0o644); err != nil {
			fatal("restore PIN", err)
		}
		emptyTools := filepath.Join(scratch, "empty-tools")
		if err := os.MkdirAll(filepath.Join(emptyTools, "bin"), 0o755); err != nil {
			fatal("mkdir empty-tools", err)
		}
		workDir := filepath.Join(scratch, "no-dbhash-work")
		res, err := runGoPkg(ctx, scratchRepo,
			"./ch03-btrees-and-page-management/experiments/3.3-same-rows-different-bytes",
			[]string{"COURSE_WORK_DIR=" + workDir, "COURSE_TOOLS_DIR=" + emptyTools})
		if err != nil {
			fatal("run 3.3 no-dbhash", err)
		}
		hasNotice := strings.Contains(res.stdout, "NO-DBHASH:")
		hasOK := strings.Contains(res.stdout, "SAME ROWS OK")
		held := res.exitCode == 0 && hasNotice && hasOK
		report("R2-06",
			"run experiment 3.3 in scratch repo with no _tools/bin/dbhash and empty COURSE_TOOLS_DIR",
			"exits 0, prints NO-DBHASH: notice, skips dbhash checks cleanly, and ends with SAME ROWS OK",
			fmt.Sprintf("exit=%d, has_NO_DBHASH=%v, has_SAME_ROWS_OK=%v", res.exitCode, hasNotice, hasOK),
			held)
	}
}
