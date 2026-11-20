package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"

	_ "modernc.org/sqlite"
)

const (
	pinnedVersionNumber = 3053004
	childReadHeader     = "child-read-header"
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

func openReadOnly(path string) *sql.DB {
	uri, err := sqlitex.FileURI(path, url.Values{"mode": {"ro"}})
	if err != nil {
		fatal("uri", err)
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		fatal("open read-only", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func queryText(ctx context.Context, q sqlitex.Querier, query string) (string, error) {
	var value any
	if err := q.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return "", err
	}
	if b, ok := value.([]byte); ok {
		return string(b), nil
	}
	return fmt.Sprint(value), nil
}

func readHeaderFromChild(path string) []byte {
	command := must(trace.SelfCommand(childReadHeader, path))("self command")
	out, err := exec.Command(command[0], command[1:]...).Output()
	if err != nil {
		fatal("child read header", err)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(raw) != dbfile.HeaderSize {
		fatal("child output", fmt.Errorf("%q: %v", out, err))
	}
	return raw
}

func childMain(path string) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	header := make([]byte, dbfile.HeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(hex.EncodeToString(header))
}

func logicalHeader(ctx context.Context, q sqlitex.Querier) []byte {
	var data []byte
	if err := q.QueryRowContext(ctx, "SELECT data FROM sqlite_dbpage WHERE pgno = 1").Scan(&data); err != nil {
		fatal("sqlite_dbpage page 1", err)
	}
	return data[:dbfile.HeaderSize]
}

func parse(raw []byte) dbfile.Header {
	return must(dbfile.ParseHeader(raw))("parse header")
}

func changedFields(before, after []byte) []string {
	var lines []string
	old, updated := parse(before).Fields(), parse(after).Fields()
	for i, field := range old {
		if bytes.Equal(before[field.Offset:field.Offset+field.Size], after[field.Offset:field.Offset+field.Size]) {
			continue
		}
		var offsets []string
		for b := field.Offset; b < field.Offset+field.Size; b++ {
			if before[b] != after[b] {
				offsets = append(offsets, strconv.Itoa(b))
			}
		}
		span := fmt.Sprintf("bytes %d-%d", field.Offset, field.Offset+field.Size-1)
		if field.Size == 1 {
			span = fmt.Sprintf("byte %d", field.Offset)
		}
		lines = append(lines, fmt.Sprintf("%s %s: %s -> %s (changed bytes %s)", span, field.Name, field.Value, updated[i].Value, strings.Join(offsets, ",")))
	}
	if len(lines) == 0 {
		lines = append(lines, "no header byte changed")
	}
	return lines
}

func printChanges(prefix string, before, after []byte) {
	for _, line := range changedFields(before, after) {
		fmt.Printf("%s%s\n", prefix, line)
	}
}

func counters(raw []byte) (uint32, uint32) {
	return binary.BigEndian.Uint32(raw[24:28]), binary.BigEndian.Uint32(raw[92:96])
}

func copyFile(from, to string) {
	data := must(os.ReadFile(from))("read " + filepath.Base(from))
	if err := os.WriteFile(to, data, 0o644); err != nil {
		fatal("write copy", err)
	}
}

func walSize(path string) int64 {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		return -1
	}
	return info.Size()
}

func walText(path string) string {
	size := walSize(path)
	if size < 0 {
		return "no -wal file"
	}
	return fmt.Sprintf("-wal %d bytes", size)
}

type fieldRow struct {
	name    string
	ours    string
	pragma  string
	cliName string
}

func parseDBInfo(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) > 0 {
			values[strings.TrimSpace(name)] = fields[0]
		}
	}
	return values
}

func partA(ctx context.Context, dir string, cli *sqlitex.CLI) {
	fmt.Println()
	fmt.Println("== A. decode the header of the R0 database and compare every field with SQLite")
	r := recipe.Default()
	path := filepath.Join(dir, "r0.db")
	if err := r.Build(ctx, path, r.Steps); err != nil {
		fatal("build R0", err)
	}
	sum := must(equal.FileSHA256(path))("hash R0")
	fmt.Printf("R0: %s, built to step %d; sha256 %s\n", r, r.Steps, sum)
	f := must(dbfile.Open(path))("parse R0")
	h := f.Header
	pageCount, source := f.PageCount(), f.PageCountSource()
	f.Close()
	fmt.Println("the 100-byte header, decoded by internal/dbfile:")
	for _, field := range h.Fields() {
		fmt.Printf("  %3d %2d  %-32s %s\n", field.Offset, field.Size, field.Name, field.Value)
	}
	fmt.Printf("page count %d, taken from the %s (in-header size valid: %v)\n", pageCount, source, h.InHeaderSizeValid())

	db := must(r.Open(ctx, path))("open R0")
	pragma := func(name string) string {
		return must(queryText(ctx, db, "PRAGMA "+name))("PRAGMA " + name)
	}
	autoVacuumNames := map[string]string{"0": "NONE", "1": "FULL", "2": "INCREMENTAL"}
	rows := []fieldRow{
		{"page size", fmt.Sprint(h.PageSize), pragma("page_size"), "database page size"},
		{"write version", fmt.Sprint(h.WriteVersion), "-", "write format"},
		{"read version", fmt.Sprint(h.ReadVersion), "-", "read format"},
		{"reserved bytes", fmt.Sprint(h.ReservedBytes), "-", "reserved bytes"},
		{"file change counter", fmt.Sprint(h.ChangeCounter), "-", "file change counter"},
		{"page count", fmt.Sprint(pageCount), pragma("page_count"), "database page count"},
		{"freelist page count", fmt.Sprint(h.FreelistCount), pragma("freelist_count"), "freelist page count"},
		{"schema cookie", fmt.Sprint(h.SchemaCookie), pragma("schema_version"), "schema cookie"},
		{"schema format", fmt.Sprint(h.SchemaFormat), "-", "schema format"},
		{"default cache size", fmt.Sprint(h.DefaultCacheSize), "-", "default cache size"},
		{"auto_vacuum", h.AutoVacuumName(), autoVacuumNames[pragma("auto_vacuum")], ""},
		{"largest root page", fmt.Sprint(h.LargestRootPage), "-", "autovacuum top root"},
		{"incremental vacuum", fmt.Sprint(h.IncrementalVacuum), "-", "incremental vacuum"},
		{"text encoding", h.EncodingName(), pragma("encoding"), ""},
		{"text encoding number", fmt.Sprint(h.TextEncoding), "-", "text encoding"},
		{"user version", fmt.Sprint(h.UserVersion), pragma("user_version"), "user version"},
		{"application id", fmt.Sprint(h.ApplicationID), pragma("application_id"), "application id"},
		{"version-valid-for", fmt.Sprint(h.VersionValidFor), "-", ""},
		{"SQLite version number", fmt.Sprint(h.SQLiteVersion), "-", "software version"},
	}
	db.Close()
	if err := recipe.CheckAtRest(path); err != nil {
		fatal("R0 after pragmas", err)
	}
	check(must(equal.FileSHA256(path))("rehash") == sum, "opening R0, reading the pragmas and closing left it byte-identical and at rest")

	dbinfo := map[string]string{}
	cliStatus := "no pinned sqlite3 CLI found; the .dbinfo column is '?' and is not checked"
	if cli != nil {
		out, err := cli.Exec(ctx, path, ".dbinfo\n")
		if err != nil {
			fatal("CLI .dbinfo", err)
		}
		dbinfo = parseDBInfo(out)
		cliStatus = "pinned sqlite3 CLI .dbinfo"
	}
	fmt.Printf("oracles: PRAGMA through the Go driver; %s\n", cliStatus)
	fmt.Printf("  %-22s %-12s %-12s %-12s %s\n", "field", "ours", "PRAGMA", ".dbinfo", "match")
	allMatch := true
	for _, row := range rows {
		cliValue := "-"
		if row.cliName != "" {
			cliValue = "?"
			if cli != nil {
				cliValue = dbinfo[row.cliName]
			}
		}
		match := (row.pragma == "-" || row.pragma == row.ours) && (cliValue == "-" || cliValue == "?" || cliValue == row.ours)
		allMatch = allMatch && match
		fmt.Printf("  %-22s %-12s %-12s %-12s %v\n", row.name, row.ours, row.pragma, cliValue, match)
	}
	check(allMatch, "every decoded header field equals SQLite's own report of it")
	check(h.SQLiteVersion == pinnedVersionNumber, fmt.Sprintf("bytes 96-99 read %d (the library that last wrote the file)", pinnedVersionNumber))
	check(h.WALMode(), "bytes 18 and 19 are 2 and 2 in a WAL-mode file")
	check(h.ChangeCountersAgree() && source == dbfile.PageCountFromHeader, "bytes 24-27 equal 92-95, so the in-header size at 28 is used")
}

func buildBase(ctx context.Context, dir, name, mode string, steps int) string {
	path := filepath.Join(dir, name)
	if err := recipe.Default().WithJournalMode(mode).Build(ctx, path, steps); err != nil {
		fatal("build "+name, err)
	}
	return path
}

func partB(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== B. rollback mode: what one commit changes in page 1 (the header)")
	r := recipe.Default().WithJournalMode("delete")
	base := buildBase(ctx, dir, "rollback-base.db", "delete", 27)
	fmt.Println("base: rollback-mode recipe built to step 27, closed")
	fmt.Println("B1. steps 28, 29, 30 on ONE connection; page 1 read from the database file after each commit:")
	path := filepath.Join(dir, "rollback-one-connection.db")
	copyFile(base, path)
	before := readHeaderFromChild(path)
	db := must(r.Open(ctx, path))("open")
	advancedEveryCommit := true
	for step := 28; step <= 30; step++ {
		if err := r.ApplyStep(ctx, db, step); err != nil {
			fatal("apply", err)
		}
		after := readHeaderFromChild(path)
		check(bytes.Equal(after, logicalHeader(ctx, db)), fmt.Sprintf("after step %d the file's page 1 equals SQLite's current page 1 (no -wal in rollback mode)", step))
		fmt.Printf("  commit of step %d:\n", step)
		printChanges("    ", before, after)
		oldCounter, oldValid := counters(before)
		newCounter, newValid := counters(after)
		advancedEveryCommit = advancedEveryCommit && newCounter == oldCounter+1 && newValid == oldValid+1 && newCounter == newValid
		before = after
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	check(advancedEveryCommit, "rollback mode: bytes 24-27 and 92-95 each advance by exactly one per commit, on one connection")
	final := readHeaderFromChild(path)
	check(bytes.Equal(final, before), "closing the connection changed nothing in page 1")
}

func partC(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== C. WAL mode (the platform default): what changes in page 1, and when")
	r := recipe.Default()
	base := buildBase(ctx, dir, "wal-base.db", "wal", 27)
	baseHeader := readHeaderFromChild(base)
	c0, _ := counters(baseHeader)
	fmt.Printf("base: WAL-mode recipe built to step 27, closed; file change counter %d\n", c0)
	fmt.Println("Two copies of page 1 are reported: 'current' = SQLite's view (sqlite_dbpage, WAL included); 'file' = the bytes in the database file (read by a child process).")

	fmt.Println("C1. steps 28, 29, 30 on ONE connection (each step writes page 1: user_version):")
	path := filepath.Join(dir, "wal-one-connection.db")
	copyFile(base, path)
	db := must(r.Open(ctx, path))("open")
	previous := logicalHeader(ctx, db)
	var currentCounters []uint32
	fileUnchanged := true
	for step := 28; step <= 30; step++ {
		if err := r.ApplyStep(ctx, db, step); err != nil {
			fatal("apply", err)
		}
		current := logicalHeader(ctx, db)
		file := readHeaderFromChild(path)
		fmt.Printf("  commit of step %d (%s):\n", step, walText(path))
		printChanges("    current vs previous commit: ", previous, current)
		printChanges("    file vs base: ", baseHeader, file)
		counter, valid := counters(current)
		currentCounters = append(currentCounters, counter)
		check(counter == valid, fmt.Sprintf("step %d: the current page 1 has 24-27 equal to 92-95 (%d)", step, counter))
		fileUnchanged = fileUnchanged && bytes.Equal(file, baseHeader)
		previous = current
	}
	check(currentCounters[0] == c0+1 && currentCounters[1] == c0+1 && currentCounters[2] == c0+1, fmt.Sprintf("WAL, one connection: every commit writes the same change counter %d (base + 1), it does not advance per commit", c0+1))
	check(fileUnchanged, "WAL: the database file's page 1 does not change at commit; the new page 1 is a frame in the -wal")
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	afterClose := readHeaderFromChild(path)
	fmt.Printf("  after the last connection closed (%s):\n", walText(path))
	printChanges("    file vs base: ", baseHeader, afterClose)
	check(bytes.Equal(afterClose, previous), "the checkpoint at close copied the current page 1 into the file")

	fmt.Println("C2. three commits on ONE connection that do not touch page 1 (UPDATE one account balance):")
	path = filepath.Join(dir, "wal-no-page1.db")
	copyFile(base, path)
	db = must(r.Open(ctx, path))("open")
	start := logicalHeader(ctx, db)
	unchanged := true
	for i := 1; i <= 3; i++ {
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1"); err != nil {
			fatal("update", err)
		}
		current := logicalHeader(ctx, db)
		fmt.Printf("  commit %d (%s): ", i, walText(path))
		printChanges("current vs base: ", start, current)
		unchanged = unchanged && bytes.Equal(current, start)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	afterClose = readHeaderFromChild(path)
	check(unchanged && bytes.Equal(afterClose, baseHeader), "WAL: commits that do not write page 1 leave every header byte unchanged, even the change counter, before and after the checkpoint")

	fmt.Println("C3. steps 28, 29, 30 each on a NEW connection while a second idle connection stays open (so closing a writer does not checkpoint):")
	path = filepath.Join(dir, "wal-new-connections.db")
	copyFile(base, path)
	holder := must(r.Open(ctx, path))("open holder")
	previous = logicalHeader(ctx, holder)
	var perSession []uint32
	for step := 28; step <= 30; step++ {
		writer := must(r.Open(ctx, path))("open writer")
		if err := r.ApplyStep(ctx, writer, step); err != nil {
			fatal("apply", err)
		}
		if err := writer.Close(); err != nil {
			fatal("close writer", err)
		}
		current := logicalHeader(ctx, holder)
		fmt.Printf("  step %d on its own connection (%s):\n", step, walText(path))
		printChanges("    current vs previous commit: ", previous, current)
		counter, _ := counters(current)
		perSession = append(perSession, counter)
		previous = current
	}
	file := readHeaderFromChild(path)
	check(bytes.Equal(file, baseHeader), "WAL: with another connection open, the file's page 1 still equals the base after three commits")
	check(perSession[0] == c0+1 && perSession[1] == c0+2 && perSession[2] == c0+3, "WAL: each new connection reads page 1 afresh and writes counter + 1, so the counter advances once per connection that writes page 1")
	if err := holder.Close(); err != nil {
		fatal("close holder", err)
	}
	check(bytes.Equal(readHeaderFromChild(path), previous) && walSize(path) == -1, "closing the last connection checkpointed page 1 into the file and removed the -wal")
}

func partD(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== D. bytes 18-19: switching journal mode rewrites the header")
	base := buildBase(ctx, dir, "switch.db", "delete", 3)
	before := readHeaderFromChild(base)
	fmt.Printf("base: rollback-mode recipe built to step 3; bytes 18,19 = %d,%d\n", before[18], before[19])
	plain := func(statement string) {
		uri := must(sqlitex.FileURI(base, nil))("uri")
		db := must(sql.Open("sqlite", uri))("open")
		db.SetMaxOpenConns(1)
		mode := must(queryText(ctx, db, statement))(statement)
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		fmt.Printf("  %s -> %s; connection closed (%s)\n", statement, mode, walText(base))
	}
	plain("PRAGMA journal_mode=WAL")
	afterWAL := readHeaderFromChild(base)
	printChanges("    file vs before: ", before, afterWAL)
	check(afterWAL[18] == 2 && afterWAL[19] == 2, "PRAGMA journal_mode=WAL sets bytes 18 and 19 to 2")
	plain("PRAGMA journal_mode=DELETE")
	afterDelete := readHeaderFromChild(base)
	printChanges("    file vs WAL: ", afterWAL, afterDelete)
	check(afterDelete[18] == 1 && afterDelete[19] == 1, "PRAGMA journal_mode=DELETE sets bytes 18 and 19 back to 1")
}

func partE(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== E. the in-header size (bytes 28-31) is used only when bytes 24-27 equal 92-95")
	base := buildBase(ctx, dir, "size-base.db", "delete", 3)
	image := must(os.ReadFile(base))("read")
	filePages := len(image) / 4096
	fmt.Printf("base: rollback-mode recipe built to step 3: %d pages in the file, in-header size %d\n", filePages, binary.BigEndian.Uint32(image[28:32]))
	variants := []struct {
		name     string
		size     uint32
		disagree bool
	}{
		{"in-header size = file pages - 1, counters agree", uint32(filePages - 1), false},
		{"in-header size = file pages - 1, byte 95 changed so counters disagree", uint32(filePages - 1), true},
		{"in-header size = file pages + 5, counters agree", uint32(filePages + 5), false},
		{"in-header size = 0, counters agree", 0, false},
	}
	for i, v := range variants {
		edited := append([]byte(nil), image...)
		binary.BigEndian.PutUint32(edited[28:32], v.size)
		if v.disagree {
			edited[95]++
		}
		path := filepath.Join(dir, fmt.Sprintf("size-%d.db", i+1))
		if err := os.WriteFile(path, edited, 0o644); err != nil {
			fatal("write", err)
		}
		ours := "error"
		f, err := dbfile.FromBytes(edited)
		if err == nil {
			ours = fmt.Sprintf("%d (from %s)", f.PageCount(), f.PageCountSource())
		} else if strings.Contains(err.Error(), dbfile.ErrCorrupt.Error()) {
			ours = "error: in-header size larger than the file"
		}
		db := openReadOnly(path)
		theirs, qerr := queryText(ctx, db, "PRAGMA page_count")
		db.Close()
		if qerr != nil {
			theirs = "error: " + qerr.Error()
		}
		fmt.Printf("  E%d %s:\n    ours:   page count %s\n    SQLite: page_count %s\n", i+1, v.name, ours, theirs)
		switch i {
		case 0:
			check(theirs == fmt.Sprint(filePages-1) && f != nil && f.PageCount() == filePages-1, "agreeing counters: SQLite and our parser both trust the in-header size")
		case 1:
			check(theirs == fmt.Sprint(filePages) && f != nil && f.PageCount() == filePages, "disagreeing counters: both ignore bytes 28-31 and use the file size")
		case 2:
			check(qerr != nil && strings.Contains(qerr.Error(), "malformed") && err != nil, "an agreeing in-header size larger than the file is corruption to both")
		case 3:
			check(theirs == fmt.Sprint(filePages) && f != nil && f.PageCount() == filePages, "an in-header size of 0 is ignored by both")
		}
	}
}

func partF(ctx context.Context, dir string) {
	fmt.Println()
	fmt.Println("== F. page size is stored in bytes 16-17; 65536 does not fit in two bytes and is stored as 1")
	for _, size := range []int{512, 4096, 65536} {
		path := filepath.Join(dir, fmt.Sprintf("page-size-%d.db", size))
		options := sqlitex.DefaultOptions()
		options.File.PageSize = size
		options.SingleConnection = true
		db := must(sqlitex.Open(ctx, path, options))("open")
		if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
			fatal("create", err)
		}
		pragma := must(queryText(ctx, db, "PRAGMA page_size"))("page_size")
		if err := db.Close(); err != nil {
			fatal("close", err)
		}
		raw := readHeaderFromChild(path)
		h := parse(raw)
		fmt.Printf("  PRAGMA page_size=%-5d bytes 16-17 = %02x %02x (stored %d), decoded %d, SQLite reports %s\n", size, raw[16], raw[17], binary.BigEndian.Uint16(raw[16:18]), h.PageSize, pragma)
		check(fmt.Sprint(h.PageSize) == pragma, fmt.Sprintf("page size %d decodes to what SQLite reports", size))
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == childReadHeader {
		childMain(os.Args[2])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	var cli *sqlitex.CLI
	if located, err := sqlitex.LocateCLI(ctx); err == nil {
		cli = &located
		fmt.Println(located.PinLine())
		fmt.Printf("INFO: CLI %s\n", located.Path)
	} else {
		fmt.Printf("NO-CLI: %v; the .dbinfo cross-check is skipped\n", err)
	}
	dir := sqlitex.MustWorkDir("2.1")
	fmt.Printf("INFO: work dir %s\n", dir)
	partA(ctx, dir, cli)
	partB(ctx, dir)
	partC(ctx, dir)
	partD(ctx, dir)
	partE(ctx, dir)
	partF(ctx, dir)
	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("HEADER FAILED: %d claim(s) did not hold\n", len(failures))
		for _, f := range failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("HEADER OK")
}
