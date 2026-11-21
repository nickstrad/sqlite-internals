package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/nickstrad/walfleet/internal/journalfile"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const childSpillHold = "child-spill-hold"

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

func workDir() string {
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	dir := filepath.Join(root, "_work", "adv-ch04", "journal-parser")
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	return dir
}

func rollbackOpts(pageSize int, sync string, cacheSize int) sqlitex.Options {
	o := sqlitex.DefaultOptions()
	o.File.PageSize = pageSize
	o.File.JournalMode = sqlitex.JournalDelete
	o.Conn.Synchronous = sync
	o.Conn.CacheSize = cacheSize
	o.Conn.SecureDelete = true
	o.SingleConnection = true
	return o
}

// buildSimpleDB creates a rollback-mode database with rows of ~3500 bytes (or
// smaller for 512-byte pages) so each row occupies roughly one leaf page.
func buildSimpleDB(ctx context.Context, path string, pageSize, rows, bodyLen int) {
	if err := recipe.RemoveDatabase(path); err != nil {
		fatal("remove db", err)
	}
	db, err := sqlitex.Open(ctx, path, rollbackOpts(pageSize, sqlitex.SyncFull, -2000))
	if err != nil {
		fatal("open db", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		fatal("create table", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	rng := rand.New(rand.NewPCG(401, uint64(pageSize)))
	buf := make([]byte, bodyLen)
	for id := 1; id <= rows; id++ {
		for i := range buf {
			buf[i] = byte(rng.Uint32())
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO t(id, body) VALUES(?, ?)", id, buf); err != nil {
			fatal("insert", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func buildJournalHeader(sector, pageSize int, magic bool, nRec, nonce, initialPages uint32) []byte {
	hdr := make([]byte, sector)
	if magic {
		copy(hdr[0:8], journalfile.Magic[:])
	}
	binary.BigEndian.PutUint32(hdr[8:12], nRec)
	binary.BigEndian.PutUint32(hdr[12:16], nonce)
	binary.BigEndian.PutUint32(hdr[16:20], initialPages)
	binary.BigEndian.PutUint32(hdr[20:24], uint32(sector))
	binary.BigEndian.PutUint32(hdr[24:28], uint32(pageSize))
	return hdr
}

func appendJournalRecord(buf []byte, page uint32, image []byte, nonce uint32) []byte {
	buf = binary.BigEndian.AppendUint32(buf, page)
	buf = append(buf, image...)
	buf = binary.BigEndian.AppendUint32(buf, journalfile.Checksum(nonce, image))
	return buf
}

// appendSuperJournal appends a SQLite super-journal footer using either signed
// int8 byte summation (matching pager.c:1736 on x86_64) or unsigned uint8 summation.
func appendSuperJournal(buf []byte, pageSize uint32, name []byte, signedSum bool) []byte {
	buf = binary.BigEndian.AppendUint32(buf, journalfile.SuperJournalPage(pageSize))
	buf = append(buf, name...)
	var sum uint32
	for _, b := range name {
		if signedSum {
			sum += uint32(int32(int8(b)))
		} else {
			sum += uint32(b)
		}
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(name)))
	buf = binary.BigEndian.AppendUint32(buf, sum)
	buf = append(buf, journalfile.Magic[:]...)
	return buf
}

func queryPageCount(ctx context.Context, path string) (int, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// JRN-01 attacks internal/journalfile on sub-sector journal files (28..511 bytes):
// pager.c:1608 (readJournalHdr) requires journalSize >= sectorSize (512) before
// accepting a header or truncating, whereas journalfile.Parse checks end < 28,
// sets a negative TrailingByte (end - sector), and predicts TruncateTo = InitialPages.
func attackJRN01(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	dbPath := filepath.Join(dir, "t.db")
	buildSimpleDB(ctx, dbPath, 4096, 6, 3500)
	origPages, err := queryPageCount(ctx, dbPath)
	if err != nil {
		fatal("page_count", err)
	}

	// Build a 512-byte synced header with InitialPages=1 and truncate it to 100 bytes.
	fullHdr := buildJournalHeader(512, 4096, true, 1, 0x11223344, 1)
	shortHdr := fullHdr[:100]
	j, err := journalfile.Parse(shortHdr)
	if err != nil {
		fatal("parse short", err)
	}
	pb := j.Playback()

	// Write that exact 100-byte journal beside the DB and let real SQLite reopen it.
	if err := os.WriteFile(dbPath+"-journal", shortHdr, 0o644); err != nil {
		fatal("write short journal", err)
	}
	afterPages, err := queryPageCount(ctx, dbPath)
	if err != nil {
		fatal("reopen short", err)
	}
	_, jErr := os.Stat(dbPath + "-journal")
	jGone := os.IsNotExist(jErr)

	// Also test 28-byte not-yet-synced header for negative TrailingByte.
	coldShort := buildJournalHeader(512, 4096, false, 0, 0x55667788, 1)[:28]
	jc, err := journalfile.Parse(coldShort)
	if err != nil {
		fatal("parse cold short", err)
	}

	held := j.TrailingByte >= 0 && jc.TrailingByte >= 0 && (pb.TruncateTo == 0 || int(pb.TruncateTo) == afterPages)
	report(
		"JRN-01",
		"100-byte hot journal (< 512B sector) and 28-byte cold journal passed to journalfile.Parse/Playback vs real SQLite",
		"TrailingByte >= 0 and Playback().TruncateTo matches SQLite (no truncation when file < sectorSize)",
		fmt.Sprintf("hot TrailingByte=%d cold TrailingByte=%d Playback.TruncateTo=%d SQLite pages %d->%d journalDeleted=%v",
			j.TrailingByte, jc.TrailingByte, pb.TruncateTo, origPages, afterPages, jGone),
		held,
	)
}

// JRN-02a tests super-journal footer parsing: signed int8 checksum on non-ASCII
// UTF-8 paths (matching pager.c:1736 on x86_64) and rejection of NUL-leading names
// (pager.c:1337 checks zOut[0]==0).
func attackJRN02a(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn02a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	dbPath := filepath.Join(dir, "nonascii.db")
	buildSimpleDB(ctx, dbPath, 4096, 3, 3500)
	origBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read db", err)
	}
	origPage2 := slices.Clone(origBytes[4096:8192])

	// Mutate page 2 in the DB (keep page 2 header valid by flipping a payload byte in row 1).
	modDB := slices.Clone(origBytes)
	modDB[4096+3000] ^= 0xff
	if err := os.WriteFile(dbPath, modDB, 0o644); err != nil {
		fatal("write mod db", err)
	}

	// Build a valid hot journal restoring page 2, with a non-ASCII UTF-8 super-journal
	// name whose file does NOT exist. Use signed int8 checksum (what SQLite on x86_64 uses).
	superName := []byte(filepath.Join(dir, "caf\xc3\xa9-mj01"))
	jrnl := buildJournalHeader(512, 4096, true, 1, 0x12345678, uint32(len(origBytes)/4096))
	jrnl = appendJournalRecord(jrnl, 2, origPage2, 0x12345678)
	jrnl = appendSuperJournal(jrnl, 4096, superName, true)

	parsed, err := journalfile.Parse(jrnl)
	if err != nil {
		fatal("parse nonascii super", err)
	}

	// Confirm real SQLite recognizes the signed-checksum non-ASCII super-journal:
	// because the super-journal file does NOT exist, SQLite must NOT roll back page 2!
	if err := os.WriteFile(dbPath+"-journal", jrnl, 0o644); err != nil {
		fatal("write journal", err)
	}
	if _, err := queryPageCount(ctx, dbPath); err != nil {
		fatal("reopen nonascii", err)
	}
	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read after", err)
	}
	sqliteRecognizedNonASCII := bytes.Equal(afterBytes, modDB)

	// Also check NUL-leading super-journal name ("\x00\x00", checksum 0):
	// pager.c:1337 rejects zOut[0]==0 so SQLite rolls back; journalfile must ignore it.
	nulJrnl := buildJournalHeader(512, 4096, true, 1, 0x12345678, uint32(len(origBytes)/4096))
	nulJrnl = appendJournalRecord(nulJrnl, 2, origPage2, 0x12345678)
	nulJrnl = appendSuperJournal(nulJrnl, 4096, []byte{0, 0}, true)
	nulParsed, err := journalfile.Parse(nulJrnl)
	if err != nil {
		fatal("parse nul super", err)
	}
	// If journalfile erroneously accepted the NUL name, TrailingByte becomes 0 and
	// RecordsPresent stays 1 (same as valid footer), whereas rejecting it leaves TrailingByte=22.
	nulRejectedByParser := nulParsed.TrailingByte > 0

	held := parsed.SuperJournal == string(superName) && nulRejectedByParser
	report(
		"JRN-02a",
		"non-ASCII UTF-8 super-journal checksum (signed int8 in pager.c) and NUL-leading super-journal name",
		fmt.Sprintf("journalfile parses non-ASCII SuperJournal (matching SQLite recognized=%v) and rejects NUL-leading footer", sqliteRecognizedNonASCII),
		fmt.Sprintf("SQLite recognized non-ASCII=%v, journalfile.SuperJournal=%q (trailing=%d); NUL-leading rejected=%v (trailing=%d)",
			sqliteRecognizedNonASCII, parsed.SuperJournal, parsed.TrailingByte, nulRejectedByParser, nulParsed.TrailingByte),
		held,
	)
}

// JRN-02b tests Playback() when a valid ASCII super-journal pointer references a
// super-journal file that does not exist on disk (meaning a multi-DB commit already
// passed its commit point): SQLite skips rollback (pager.c:2892-2897).
func attackJRN02b(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn02b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	dbPath := filepath.Join(dir, "ascii.db")
	buildSimpleDB(ctx, dbPath, 4096, 3, 3500)
	origBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read db", err)
	}
	origPage2 := slices.Clone(origBytes[4096:8192])
	modDB := slices.Clone(origBytes)
	modDB[4096+3000] ^= 0xff
	if err := os.WriteFile(dbPath, modDB, 0o644); err != nil {
		fatal("write mod db", err)
	}

	superName := []byte(filepath.Join(dir, "gone-mj01"))
	jrnl := buildJournalHeader(512, 4096, true, 1, 0xabcdef01, uint32(len(origBytes)/4096))
	jrnl = appendJournalRecord(jrnl, 2, origPage2, 0xabcdef01)
	jrnl = appendSuperJournal(jrnl, 4096, superName, true)

	parsed, err := journalfile.Parse(jrnl)
	if err != nil {
		fatal("parse", err)
	}
	pb := parsed.Playback()

	if err := os.WriteFile(dbPath+"-journal", jrnl, 0o644); err != nil {
		fatal("write journal", err)
	}
	if _, err := queryPageCount(ctx, dbPath); err != nil {
		fatal("reopen", err)
	}
	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read after", err)
	}
	sqliteRolledBack := bytes.Equal(afterBytes, origBytes)

	// Playback() operates on bytes alone without checking super-journal existence, so
	// check whether it at least notes or guards super-journal dependence.
	held := parsed.SuperJournal == string(superName) && !sqliteRolledBack && len(pb.Pages) == 0
	report(
		"JRN-02b",
		"hot journal with valid ASCII super-journal pointer whose file is absent on disk",
		"Playback() matches SQLite (no pages rolled back when super-journal is absent, or refuses unconditional rollback prediction)",
		fmt.Sprintf("SuperJournal=%q SQLite rolledBack=%v Playback.Pages=%v StopReason=%q",
			filepath.Base(parsed.SuperJournal), sqliteRolledBack, pb.Pages, pb.StopReason),
		held,
	)
}

// JRN-03a checks journalfile.Parse and Checksum against real live SQLite journals
// across all 8 valid SQLite page sizes (512..65536).
func attackJRN03a(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn03a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	pageSizes := []int{512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}
	var mismatches []string
	for _, ps := range pageSizes {
		path := filepath.Join(dir, fmt.Sprintf("ps-%d.db", ps))
		bodyLen := min(ps/2, 3500)
		buildSimpleDB(ctx, path, ps, 8, bodyLen)
		before, err := os.ReadFile(path)
		if err != nil {
			fatal("read before", err)
		}
		origPages := uint32(len(before) / ps)

		db, err := sqlitex.Open(ctx, path, rollbackOpts(ps, sqlitex.SyncOff, -2000))
		if err != nil {
			fatal("open", err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fatal("begin", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE t SET body = zeroblob(?) WHERE id <= 4", bodyLen); err != nil {
			fatal("update", err)
		}
		// Reading -journal is safe: SQLite holds POSIX locks on the .db, not -journal.
		j, err := journalfile.ReadFile(path + "-journal")
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("ps=%d read err=%v", ps, err))
			tx.Rollback()
			db.Close()
			continue
		}
		h := j.Segments[0].Header
		if h.PageSize != uint32(ps) || h.InitialPages != origPages || j.RecordsPresent() == 0 || j.ChecksumFailures() != 0 {
			mismatches = append(mismatches, fmt.Sprintf("ps=%d hdr=%+v recs=%d bad=%d", ps, h, j.RecordsPresent(), j.ChecksumFailures()))
		} else {
			for _, r := range j.Segments[0].Records {
				start := int(r.Page-1) * ps
				if !bytes.Equal(before[start:start+ps], r.Image) {
					mismatches = append(mismatches, fmt.Sprintf("ps=%d page %d image mismatch", ps, r.Page))
					break
				}
			}
		}
		tx.Rollback()
		db.Close()
	}
	held := len(mismatches) == 0
	got := fmt.Sprintf("all %d page sizes (512..65536) parsed with 0 checksum failures and exact pre-image match", len(pageSizes))
	if !held {
		got = strings.Join(mismatches, "; ")
	}
	report(
		"JRN-03a",
		"live SQLite rollback journals across all 8 valid page sizes (512..65536) parsed by internal/journalfile",
		"every page size yields valid header, 0 checksum failures, and byte-exact pre-tx page images",
		got,
		held,
	)
}

// JRN-03b probes pager_cksum's 20-of-4096 sparse checksum coverage: flipping a
// sampled byte (pageSize-200 = 3896) halts playback in both journalfile and SQLite,
// while flipping an unsampled byte inside a row payload passes both checksums,
// silently corrupts the restored DB, and passes PRAGMA integrity_check.
func attackJRN03b(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn03b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	dbPath := filepath.Join(dir, "base.db")
	if err := recipe.RemoveDatabase(dbPath); err != nil {
		fatal("remove", err)
	}
	db, err := sqlitex.Open(ctx, dbPath, rollbackOpts(4096, sqlitex.SyncFull, -2000))
	if err != nil {
		fatal("open", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, body TEXT NOT NULL)"); err != nil {
		fatal("create", err)
	}
	payload := strings.Repeat("A", 3000)
	if _, err := db.ExecContext(ctx, "INSERT INTO t(id, body) VALUES(1, ?)", payload); err != nil {
		fatal("insert", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	origBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read", err)
	}
	origPage2 := slices.Clone(origBytes[4096:8192])

	// Find an offset inside the "AAA..." payload on page 2 that is NOT sampled by
	// pager_cksum (sampled offsets satisfy (4096 - off) % 200 == 0 && off > 0).
	unsampledOff := -1
	for off := 100; off < 4096; off++ {
		if origPage2[off] == 'A' && (4096-off)%200 != 0 {
			unsampledOff = off
			break
		}
	}
	if unsampledOff < 0 {
		fatal("find unsampled offset", fmt.Errorf("no 'A' payload byte found on page 2"))
	}

	// Build a modified DB where page 2 has a different payload ('B') so rollback is observable.
	modDB := slices.Clone(origBytes)
	modDB[4096+unsampledOff] = 'B'

	// Case 1: Flip sampled byte 3896 inside the journal record -> checksum MUST fail.
	sampledJrnl := buildJournalHeader(512, 4096, true, 1, 0x42424242, uint32(len(origBytes)/4096))
	sampledJrnl = appendJournalRecord(sampledJrnl, 2, origPage2, 0x42424242)
	sampledJrnl[512+4+3896] ^= 0x01
	js, _ := journalfile.Parse(sampledJrnl)
	pbs := js.Playback()

	// Case 2: Flip unsampled payload byte inside the journal record ('A' -> 'Z').
	unsampledJrnl := buildJournalHeader(512, 4096, true, 1, 0x42424242, uint32(len(origBytes)/4096))
	unsampledJrnl = appendJournalRecord(unsampledJrnl, 2, origPage2, 0x42424242)
	unsampledJrnl[512+4+unsampledOff] = 'Z'
	ju, _ := journalfile.Parse(unsampledJrnl)
	pbu := ju.Playback()

	// Replay Case 2 with real SQLite.
	testDB := filepath.Join(dir, "unsampled.db")
	if err := os.WriteFile(testDB, modDB, 0o644); err != nil {
		fatal("write mod", err)
	}
	if err := os.WriteFile(testDB+"-journal", unsampledJrnl, 0o644); err != nil {
		fatal("write jrnl", err)
	}
	rdb, err := sqlitex.Open(ctx, testDB, rollbackOpts(4096, sqlitex.SyncFull, -2000))
	if err != nil {
		fatal("reopen", err)
	}
	var ic, body string
	if err := rdb.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
		fatal("ic", err)
	}
	if err := rdb.QueryRowContext(ctx, "SELECT body FROM t WHERE id = 1").Scan(&body); err != nil {
		fatal("select", err)
	}
	rdb.Close()

	silentCorrupt := ic == "ok" && strings.Contains(body, "Z") && body != payload
	// Expect: sampled byte halts rollback; unsampled byte silently slips past pager_cksum
	// (confirming pager.c:2251 sparse checksum behavior AND journalfile parity).
	held := js.ChecksumFailures() == 1 && len(pbs.Pages) == 0 && ju.ChecksumFailures() == 0 && len(pbu.Pages) == 1 && silentCorrupt
	report(
		"JRN-03b",
		fmt.Sprintf("flip sampled byte 3896 vs unsampled payload byte %d in hot journal record", unsampledOff),
		"sampled flip halts playback (1 checksum failure); unsampled flip slips past 20-byte pager_cksum and silently corrupts restored row while passing integrity_check",
		fmt.Sprintf("sampled: bad=%d pages=%v; unsampled: bad=%d pages=%v integrity_check=%s rowHasZ=%v",
			js.ChecksumFailures(), pbs.Pages, ju.ChecksumFailures(), pbu.Pages, ic, strings.Contains(body, "Z")),
		held,
	)
}

// childSpillMain opens dbPath with cache_size=10, starts a transaction, updates all
// rows to force pagerStress cache spills BEFORE COMMIT, prints the spill count and
// "READY", then blocks waiting for EOF on stdin (parent inspects files or SIGKILLs it).
func childSpillMain(dbPath, syncMode string) {
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, dbPath, rollbackOpts(4096, syncMode, 10))
	if err != nil {
		fatal("child open", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("child conn", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		fatal("child begin", err)
	}
	// Update all 55 rows with new deterministic bytes so >50 pages dirty and spill.
	newBody := bytes.Repeat([]byte{0x5a}, 3500)
	if _, err := tx.ExecContext(ctx, "UPDATE t SET body = ?", newBody); err != nil {
		fatal("child update", err)
	}
	spill, err := sqlitex.ReadDBStatus(ctx, conn, sqlitex.DBStatusCacheSpill, false)
	if err != nil {
		fatal("child spill stat", err)
	}
	fmt.Printf("READY spill=%d\n", spill.Current)
	// Wait until parent closes stdin or kills us.
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	_ = tx.Rollback()
	_ = conn.Close()
	_ = db.Close()
}

// runSpillAndKill launches childSpillMain, waits for "READY", kills the child with
// SIGKILL BEFORE COMMIT, and returns the spill count.
func runSpillAndKill(dbPath, syncMode string) int {
	exe, err := os.Executable()
	if err != nil {
		fatal("exe", err)
	}
	cmd := exec.Command(exe, childSpillHold, dbPath, syncMode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal("stdout pipe", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal("stdin pipe", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fatal("start child", err)
	}
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		fatal("wait ready", fmt.Errorf("child exited early: %v", sc.Err()))
	}
	line := sc.Text()
	if !strings.HasPrefix(line, "READY spill=") {
		fatal("unexpected child line", fmt.Errorf("%q", line))
	}
	spills, err := strconv.Atoi(strings.TrimPrefix(line, "READY spill="))
	if err != nil {
		fatal("parse spill", err)
	}
	// Kill child BEFORE COMMIT while transaction is still open and spilled.
	if err := cmd.Process.Kill(); err != nil {
		fatal("kill child", err)
	}
	_ = stdin.Close()
	_ = cmd.Wait()
	return spills
}

// JRN-04 attacks build-notes Correction 3 ("The -journal written before COMMIT is not hot")
// by forcing pagerStress cache spills (cache_size=10 on a 60-page UPDATE) before COMMIT.
func attackJRN04(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn04")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	dbPath := filepath.Join(dir, "spill.db")
	buildSimpleDB(ctx, dbPath, 4096, 55, 3500)
	preBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read pre", err)
	}

	spills := runSpillAndKill(dbPath, sqlitex.SyncFull)
	midBytes, err := os.ReadFile(dbPath)
	if err != nil {
		fatal("read mid", err)
	}
	j, err := journalfile.ReadFile(dbPath + "-journal")
	if err != nil {
		fatal("read spill journal", err)
	}
	segStates := make([]string, len(j.Segments))
	for i, s := range j.Segments {
		segStates[i] = fmt.Sprintf("seg%d:%s(%d)", i+1, s.Header.State, len(s.Records))
	}
	dbModifiedBeforeCommit := !bytes.Equal(preBytes, midBytes)

	// Corrected/narrowed claim (REPAIR-REPORT.md / build-notes.md:795-802):
	// Correction 3 ("-journal before COMMIT is not hot and DB untouched") holds only
	// when the transaction fits in cache (spill=0). With pagerStress spills, syncJournal
	// makes -journal hot (first byte 0xd9, multi-segment) and writes dirty pages to DB
	// before COMMIT.
	held := spills > 0 && j.LooksHot() && j.FirstByte == journalfile.Magic[0] && len(j.Segments) >= 2 && dbModifiedBeforeCommit
	report(
		"JRN-04",
		"55-page UPDATE with cache_size=10 (synchronous=FULL), killed BEFORE COMMIT to trigger pagerStress spill",
		"per narrowed build-notes #3: spilling transaction calls syncJournal before COMMIT (hot 0xd9, >=2 segments) and modifies DB before COMMIT",
		fmt.Sprintf("spills=%d LooksHot=%v firstByte=0x%02x segments=%v dbModifiedBeforeCommit=%v",
			spills, j.LooksHot(), j.FirstByte, segStates, dbModifiedBeforeCommit),
		held,
	)
}

// JRN-05a verifies internal/journalfile's multi-segment Parse + Playback on real
// live spilled journals (FULL multi-segment and OFF spill), and checks SQLite recovery.
func attackJRN05a(ctx context.Context, base string) {
	dir := filepath.Join(base, "jrn05a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}

	type modeRes struct {
		mode       string
		spills     int
		segments   int
		recs       int
		badCksum   int
		pbPages    int
		recovered  bool
		ic         string
		coveredAll bool
	}
	var results []modeRes

	for _, syncMode := range []string{sqlitex.SyncFull, sqlitex.SyncOff} {
		dbPath := filepath.Join(dir, "spill-"+strings.ToLower(syncMode)+".db")
		buildSimpleDB(ctx, dbPath, 4096, 55, 3500)
		preBytes, err := os.ReadFile(dbPath)
		if err != nil {
			fatal("read pre", err)
		}
		spills := runSpillAndKill(dbPath, syncMode)
		midBytes, err := os.ReadFile(dbPath)
		if err != nil {
			fatal("read mid", err)
		}
		j, err := journalfile.ReadFile(dbPath + "-journal")
		if err != nil {
			fatal("read journal", err)
		}
		pb := j.Playback()

		// Verify every page that changed in the DB file before COMMIT is covered by Playback().Pages.
		coveredAll := true
		for p := 0; p < len(preBytes); p += 4096 {
			if !bytes.Equal(preBytes[p:p+4096], midBytes[p:p+4096]) {
				pgno := uint32(p/4096 + 1)
				if !slices.Contains(pb.Pages, pgno) {
					coveredAll = false
					break
				}
			}
		}

		// Now reopen with real SQLite and verify exact recovery to preBytes.
		db, err := sqlitex.Open(ctx, dbPath, rollbackOpts(4096, sqlitex.SyncFull, -2000))
		if err != nil {
			fatal("reopen "+syncMode, err)
		}
		var ic string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ic); err != nil {
			fatal("ic", err)
		}
		db.Close()
		afterBytes, err := os.ReadFile(dbPath)
		if err != nil {
			fatal("read after", err)
		}

		results = append(results, modeRes{
			mode:       syncMode,
			spills:     spills,
			segments:   len(j.Segments),
			recs:       j.RecordsPresent(),
			badCksum:   j.ChecksumFailures(),
			pbPages:    len(pb.Pages),
			recovered:  bytes.Equal(afterBytes, preBytes),
			ic:         ic,
			coveredAll: coveredAll,
		})
	}

	allOK := true
	var parts []string
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("%s(spills=%d segs=%d recs=%d bad=%d pb=%d covered=%v exact=%v ic=%s)",
			r.mode, r.spills, r.segments, r.recs, r.badCksum, r.pbPages, r.coveredAll, r.recovered, r.ic))
		if r.spills == 0 || r.badCksum != 0 || !r.coveredAll || !r.recovered || r.ic != "ok" {
			allOK = false
		}
	}
	// Specifically require FULL to have >= 2 live segments so multi-segment parsing is truly exercised.
	if results[0].segments < 2 {
		allOK = false
	}
	report(
		"JRN-05a",
		"live multi-segment spilled journal (FULL & OFF, cache_size=10) parsed by journalfile and recovered by SQLite",
		"FULL produces >=2 segments, 0 checksum failures, Playback covers every modified DB page, and SQLite recovers exact pre-tx bytes",
		strings.Join(parts, "; "),
		allOK,
	)
}

// JRN-05b fuzzes journalfile.Parse and Playback with 5,000 seeded mutations
// (bit-flips, truncations, header-field smashes, super-journal length overflows)
// checking 0 panics and TrailingByte >= 0.
func attackJRN05b() {
	// Start from a valid 2-segment journal with a super-journal footer.
	seed := buildJournalHeader(512, 512, true, 1, 0x11112222, 4)
	seed = appendJournalRecord(seed, 1, bytes.Repeat([]byte{0xaa}, 512), 0x11112222)
	second := ((len(seed)-1)/512 + 1) * 512
	seed = append(seed, make([]byte, second-len(seed))...)
	seed = append(seed, buildJournalHeader(512, 512, true, 1, 0x33334444, 4)...)
	seed = appendJournalRecord(seed, 2, bytes.Repeat([]byte{0xbb}, 512), 0x33334444)
	seed = appendSuperJournal(seed, 512, []byte("fuzz-mj01"), false)

	const iterations = 5000
	rng := rand.New(rand.NewPCG(405, 999))
	panics := 0
	negTrailing := 0
	var firstNeg string

	for i := range iterations {
		buf := slices.Clone(seed)
		switch i % 5 {
		case 0:
			// Random truncation (including 0..511 bytes).
			cut := rng.IntN(len(buf) + 1)
			buf = buf[:cut]
		case 1:
			// Random single-byte or multi-byte bit-flips.
			flips := 1 + rng.IntN(6)
			for range flips {
				pos := rng.IntN(len(buf))
				buf[pos] ^= byte(1 << uint(rng.IntN(8)))
			}
		case 2:
			// Smash 32-bit header fields (RecordCount, SectorSize, PageSize, InitialPages).
			fieldOff := (rng.IntN(7)) * 4
			vals := []uint32{0, 1, 31, 32, 512, 65536, 0x7fffffff, 0xfffffffe, 0xffffffff}
			binary.BigEndian.PutUint32(buf[fieldOff:fieldOff+4], vals[rng.IntN(len(vals))])
		case 3:
			// Smash super-journal footer length/checksum.
			if len(buf) >= 20 {
				vals := []uint32{0, 1, 16, 10000, 0x7fffffff, 0xffffffff}
				binary.BigEndian.PutUint32(buf[len(buf)-16:len(buf)-12], vals[rng.IntN(len(vals))])
			}
		case 4:
			// Completely random short/medium garbage buffer.
			n := rng.IntN(1600)
			buf = make([]byte, n)
			for k := range buf {
				buf[k] = byte(rng.Uint32())
			}
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					panics++
				}
			}()
			j, err := journalfile.Parse(buf)
			if err == nil && j != nil {
				if j.TrailingByte < 0 {
					negTrailing++
					if firstNeg == "" {
						firstNeg = fmt.Sprintf("iter=%d len=%d TrailingByte=%d", i, len(buf), j.TrailingByte)
					}
				}
				_ = j.LooksHot()
				_ = j.RecordsPresent()
				_ = j.ChecksumFailures()
				_ = j.Playback()
			}
		}()
	}

	held := panics == 0 && negTrailing == 0
	got := fmt.Sprintf("%d iterations: panics=%d negativeTrailing=%d", iterations, panics, negTrailing)
	if firstNeg != "" {
		got += " (first: " + firstNeg + ")"
	}
	report(
		"JRN-05b",
		fmt.Sprintf("%d seeded fuzz mutations on journalfile.Parse and Playback", iterations),
		"0 panics and TrailingByte >= 0 on every mutated input",
		got,
		held,
	)
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == childSpillHold {
		childSpillMain(os.Args[2], os.Args[3])
		return
	}
	ctx := context.Background()
	base := workDir()
	defer os.RemoveAll(base)

	attackJRN01(ctx, base)
	attackJRN02a(ctx, base)
	attackJRN02b(ctx, base)
	attackJRN03a(ctx, base)
	attackJRN03b(ctx, base)
	attackJRN04(ctx, base)
	attackJRN05a(ctx, base)
	attackJRN05b()
}
