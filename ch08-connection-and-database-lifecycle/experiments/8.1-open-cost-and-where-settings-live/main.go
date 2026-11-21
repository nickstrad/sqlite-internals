package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/connmgr"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
)

const (
	packagePath    = "ch08-connection-and-database-lifecycle/experiments/8.1-open-cost-and-where-settings-live"
	pageSize       = 4096
	firstStatement = "SELECT count(*) FROM t0001"
	markPrefix     = "MARK "
	shownEvents    = 8
)

var (
	failures    []string
	tableCounts = []int{1, 100, 1000}
	phases      = []string{"open", "first statement", "close"}
)

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

func plainConnection(ctx context.Context, path string) (*sql.DB, *sql.Conn) {
	uri, err := sqlitex.FileURI(path, nil)
	if err != nil {
		fatal("uri", err)
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		fatal("sql.Open", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("open "+path, err)
	}
	return db, conn
}

func closePlain(db *sql.DB, conn *sql.Conn) {
	if err := conn.Close(); err != nil {
		fatal("close conn", err)
	}
	if err := db.Close(); err != nil {
		fatal("close db", err)
	}
}

func schemaPath(dir string, tables int) string {
	return filepath.Join(dir, fmt.Sprintf("schema-%d.db", tables))
}

func buildSchema(ctx context.Context, path string, tables int) {
	options := sqlitex.DefaultOptions()
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		fatal("open", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	for i := 1; i <= tables; i++ {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("CREATE TABLE t%04d(id INTEGER PRIMARY KEY, name TEXT NOT NULL, amount INTEGER NOT NULL, note TEXT)", i)); err != nil {
			fatal("create", err)
		}
	}
	if err := tx.Commit(); err != nil {
		fatal("commit", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	status, err := connmgr.CheckAtRest(path)
	if err != nil || !status.AtRest {
		fatal("at rest "+path, fmt.Errorf("%v %v", status, err))
	}
}

func childOpen(path string) {
	runtime.LockOSThread()
	ctx := context.Background()
	fmt.Println(markPrefix + "open")
	db, conn := plainConnection(ctx, path)
	fmt.Println(markPrefix + "first statement")
	var count int
	if err := conn.QueryRowContext(ctx, firstStatement).Scan(&count); err != nil {
		fatal("first statement", err)
	}
	fmt.Println(markPrefix + "close")
	closePlain(db, conn)
	fmt.Println(markPrefix + "done")
}

type phaseIO struct {
	events     []string
	opens      []string
	readsByDB  []trace.Syscall
	otherReads map[string]int
}

func splitPhases(log *trace.StraceLog, dir string) (map[string]*phaseIO, error) {
	result := map[string]*phaseIO{}
	current := ""
	under := trace.UnderDir(dir)
	for _, call := range log.Calls {
		if call.Name == "write" && call.FD == 1 && strings.HasPrefix(call.Data, markPrefix) {
			current = strings.TrimSuffix(strings.TrimPrefix(call.Data, markPrefix), "\n")
			if current != "done" {
				result[current] = &phaseIO{otherReads: map[string]int{}}
			}
			continue
		}
		phase, ok := result[current]
		if !ok || current == "done" || !under(call) || call.Result < 0 {
			continue
		}
		base := filepath.Base(call.Path)
		switch call.Name {
		case "open", "openat":
			phase.opens = append(phase.opens, base)
			phase.events = append(phase.events, "open "+base)
		case "pread64", "read":
			if strings.HasSuffix(base, ".db") {
				phase.readsByDB = append(phase.readsByDB, call)
				if call.Offset%pageSize == 0 && call.Length == pageSize {
					phase.events = append(phase.events, fmt.Sprintf("read page %d", call.Offset/pageSize+1))
				} else {
					phase.events = append(phase.events, fmt.Sprintf("read %d bytes at %d", call.Length, call.Offset))
				}
			} else {
				phase.otherReads[base]++
			}
		}
	}
	for _, name := range phases {
		if result[name] == nil {
			return nil, fmt.Errorf("trace has no %q marker", name)
		}
	}
	return result, nil
}

func pagesOf(reads []trace.Syscall) ([]int, bool) {
	var pages []int
	whole := true
	for _, r := range reads {
		if r.Offset%pageSize != 0 || r.Length != pageSize || r.Result != pageSize {
			whole = false
			continue
		}
		pages = append(pages, int(r.Offset/pageSize)+1)
	}
	return pages, whole
}

func ranges(pages []int) string {
	if len(pages) == 0 {
		return "none"
	}
	sorted := slices.Clone(pages)
	slices.Sort(sorted)
	var parts []string
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(sorted[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", sorted[i], sorted[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

func schemaTreePages(ctx context.Context, path string) []int {
	db, conn := plainConnection(ctx, path)
	defer closePlain(db, conn)
	rows, err := conn.QueryContext(ctx, "SELECT pageno FROM dbstat WHERE name = 'sqlite_schema' ORDER BY pageno")
	if err != nil {
		fatal("dbstat", err)
	}
	defer rows.Close()
	var pages []int
	for rows.Next() {
		var page int
		if err := rows.Scan(&page); err != nil {
			fatal("dbstat scan", err)
		}
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		fatal("dbstat rows", err)
	}
	var root int
	if err := conn.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_schema WHERE name = 't0001'").Scan(&root); err != nil {
		fatal("rootpage", err)
	}
	return append(pages, root)
}

func partA(ctx context.Context, work string) {
	fmt.Println("\n== A. what open reads and what the first statement reads ==")
	fmt.Printf("each database: WAL mode, at rest, N tables of 4 columns, all empty; first statement: %s\n", firstStatement)
	if !trace.Supported {
		trace.PrintLinuxOnly("strace read counts per phase", packagePath)
	}
	for _, tables := range tableCounts {
		path := schemaPath(work, tables)
		buildSchema(ctx, path, tables)
		info, err := os.Stat(path)
		if err != nil {
			fatal("stat", err)
		}
		fmt.Printf("\n%d table(s): file %d pages\n", tables, info.Size()/pageSize)
		measureSchemaMemory(ctx, path, tables)
		if !trace.Supported {
			continue
		}
		command, err := trace.SelfCommand("child-open", path)
		if err != nil {
			fatal("self", err)
		}
		result, err := trace.Run(ctx, trace.Options{Command: command, Syscalls: []string{"open", "openat", "pread64", "read", "write", "close"}})
		if err != nil {
			fatal("strace", err)
		}
		if result.ExitCode != 0 {
			fatal("child", fmt.Errorf("exit %d: %s%s", result.ExitCode, result.Stdout, result.Stderr))
		}
		byPhase, err := splitPhases(result.Log, work)
		if err != nil {
			fatal("phases", err)
		}
		for _, name := range phases {
			p := byPhase[name]
			shown := p.events
			more := 0
			if len(shown) > shownEvents {
				shown, more = shown[:shownEvents], len(shown)-shownEvents
			}
			listed := strings.Join(shown, ", ")
			if len(shown) == 0 {
				listed = "none"
			}
			line := fmt.Sprintf("  %-16s %d event(s): %s", name, len(p.events), listed)
			if more > 0 {
				line += fmt.Sprintf(", ... %d more", more)
			}
			fmt.Println(line)
			if pages, _ := pagesOf(p.readsByDB); len(pages) > 0 {
				fmt.Printf("  %-16s whole-page reads of the .db file: %d, distinct pages %d: %s\n", "", len(pages), len(sortedUnique(pages)), ranges(sortedUnique(pages)))
			}
			if len(p.otherReads) > 0 {
				var others []string
				for _, file := range slices.Sorted(mapsKeys(p.otherReads)) {
					others = append(others, fmt.Sprintf("%s %d", file, p.otherReads[file]))
				}
				fmt.Printf("  %-16s reads of other files: %s\n", "", strings.Join(others, ", "))
			}
		}
		open := byPhase["open"]
		check(len(open.opens) == 1 && strings.HasSuffix(open.opens[0], ".db") && len(open.readsByDB) == 1 && open.readsByDB[0].Offset == 0 && open.readsByDB[0].Length == 100 && len(open.otherReads) == 0,
			fmt.Sprintf("%d table(s): open opens only the database file and reads only its 100-byte header", tables))
		first := byPhase["first statement"]
		firstPages, _ := pagesOf(first.readsByDB)
		wantPages := schemaTreePages(ctx, path)
		check(slices.Equal(sortedUnique(firstPages), sortedUnique(wantPages)),
			fmt.Sprintf("%d table(s): the first statement reads exactly the sqlite_schema b-tree pages (%d, from dbstat) plus t0001's root page", tables, len(wantPages)-1))
		walOpen := slices.Index(first.events, "open "+filepath.Base(path)+"-wal")
		pageOneReads := 0
		for _, event := range first.events {
			if event == "read page 1" {
				pageOneReads++
			}
		}
		check(pageOneReads == 2 && len(first.events) > 0 && first.events[0] == "read page 1" && walOpen > 0 && slices.Index(first.events[walOpen:], "read page 1") > 0,
			fmt.Sprintf("%d table(s): page 1 is read twice: once before the -wal is opened (header byte 19 = 2 sends lockBtree to open the WAL) and once after", tables))
		check(len(first.otherReads) == 0, fmt.Sprintf("%d table(s): nothing is read from the -wal or -shm (the -wal is empty; the -shm is mapped, not read)", tables))
		check(slices.Contains(first.opens, filepath.Base(path)+"-wal") && slices.Contains(first.opens, filepath.Base(path)+"-shm"),
			fmt.Sprintf("%d table(s): the -wal and -shm files are opened at the first statement, not at open", tables))
	}
}

func mapsKeys(m map[string]int) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func sortedUnique(pages []int) []int {
	out := slices.Clone(pages)
	slices.Sort(out)
	return slices.Compact(out)
}

var schemaUsed = map[int][2]int{}

func measureSchemaMemory(ctx context.Context, path string, tables int) {
	db, conn := plainConnection(ctx, path)
	defer closePlain(db, conn)
	read := func() int {
		status, err := sqlitex.ReadDBStatus(ctx, conn, sqlitex.DBStatusSchemaUsed, false)
		if err != nil {
			fatal("SCHEMA_USED", err)
		}
		return status.Current
	}
	atOpen := read()
	started := time.Now()
	var count int
	if err := conn.QueryRowContext(ctx, firstStatement).Scan(&count); err != nil {
		fatal("first statement", err)
	}
	elapsed := time.Since(started)
	afterFirst := read()
	schemaUsed[tables] = [2]int{atOpen, afterFirst}
	fmt.Printf("  SCHEMA_USED: %d bytes after open, %d bytes after the first statement (%d bytes per table)\n", atOpen, afterFirst, afterFirst/tables)
	fmt.Printf("INFO: first statement on %d table(s) took %s in this process\n", tables, elapsed.Round(time.Microsecond))
}

func checkSchemaMemory() {
	growing := true
	for i, tables := range tableCounts {
		if schemaUsed[tables][0] != 0 {
			growing = false
		}
		if i > 0 && schemaUsed[tables][1] <= schemaUsed[tableCounts[i-1]][1] {
			growing = false
		}
	}
	check(growing, "SCHEMA_USED is 0 after open for every schema, and after the first statement it grows with the table count (1 < 100 < 1000)")
}

type observed struct {
	Setting    string                   `json:"setting"`
	Home       sqlitex.SettingHome      `json:"home"`
	Set        string                   `json:"set_on_original"`
	Default    string                   `json:"fresh_connection_default"`
	Copy       string                   `json:"read_on_copy"`
	VacuumInto string                   `json:"read_on_vacuum_into"`
	Placement  sqlitex.SettingPlacement `json:"placement"`
}

func readPragma(ctx context.Context, conn *sql.Conn, pragma string) string {
	var value any
	if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil {
		fatal("PRAGMA "+pragma, err)
	}
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return strings.ToLower(fmt.Sprint(value))
}

func readPragmas(ctx context.Context, path string, pragmas []string) map[string]string {
	db, conn := plainConnection(ctx, path)
	defer closePlain(db, conn)
	values := map[string]string{}
	for _, pragma := range pragmas {
		values[pragma] = readPragma(ctx, conn, pragma)
	}
	return values
}

func copyFile(from, to string) {
	in, err := os.Open(from)
	if err != nil {
		fatal("copy", err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		fatal("copy", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		fatal("copy", err)
	}
	if err := out.Close(); err != nil {
		fatal("copy", err)
	}
}

type settingCase struct {
	name   string
	pragma string
	set    string
}

func partB(ctx context.Context, work string) {
	fmt.Println("\n== B. which settings live in the file and which belong to the connection ==")
	dir := filepath.Join(work, "B")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	original := filepath.Join(dir, "original.db")
	options := sqlitex.Options{
		File:             sqlitex.FileSettings{PageSize: 8192, AutoVacuum: sqlitex.AutoVacuumIncremental, Encoding: "UTF-16le", ApplicationID: sqlitex.CourseApplicationID, UserVersion: 7, JournalMode: sqlitex.JournalWAL},
		Conn:             sqlitex.ConnSettings{Synchronous: sqlitex.SyncNormal, ForeignKeys: true, SecureDelete: true, CacheSize: -8000, WALAutocheckpoint: 200, BusyTimeout: 3 * time.Second},
		SingleConnection: true,
	}
	cases := []settingCase{
		{"page_size", "page_size", "8192"},
		{"journal_mode=wal", "journal_mode", "wal"},
		{"auto_vacuum", "auto_vacuum", "2"},
		{"encoding", "encoding", "utf-16le"},
		{"user_version", "user_version", "7"},
		{"application_id", "application_id", strconv.Itoa(int(sqlitex.CourseApplicationID))},
		{"synchronous", "synchronous", "1"},
		{"foreign_keys", "foreign_keys", "1"},
		{"cache_size", "cache_size", "-8000"},
		{"busy_timeout", "busy_timeout", "3000"},
		{"wal_autocheckpoint", "wal_autocheckpoint", "200"},
		{"secure_delete", "secure_delete", "1"},
	}
	db, err := sqlitex.Open(ctx, original, options)
	if err != nil {
		fatal("open original", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		fatal("conn", err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1, 'one')"); err != nil {
		fatal("create", err)
	}
	onOriginal := map[string]string{}
	for _, c := range cases {
		onOriginal[c.name] = readPragma(ctx, conn, c.pragma)
	}
	if err := conn.Close(); err != nil {
		fatal("close", err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
	status, err := connmgr.CheckAtRest(original)
	if err != nil {
		fatal("at rest", err)
	}
	check(status.AtRest, "the original is at rest after its last connection closed: "+status.String())
	copyPath := filepath.Join(dir, "copy.db")
	copyFile(original, copyPath)
	copied, err := connmgr.CheckAtRest(copyPath)
	if err != nil {
		fatal("hash copy", err)
	}
	check(copied.AtRest && copied.SHA256 == status.SHA256, "a plain copy of the closed file has the same sha256 (rung R5: capture = the file at rest, rebuild = copy)")
	vacuumPath := filepath.Join(dir, "vacuum-into.db")
	vdb, vconn := plainConnection(ctx, original)
	if _, err := vconn.ExecContext(ctx, "VACUUM INTO ?", vacuumPath); err != nil {
		fatal("vacuum into", err)
	}
	closePlain(vdb, vconn)
	freshPath := filepath.Join(dir, "fresh.db")
	pragmas := make([]string, len(cases))
	for i, c := range cases {
		pragmas[i] = c.pragma
	}
	defaults := readPragmas(ctx, freshPath, pragmas)
	onCopy := readPragmas(ctx, copyPath, pragmas)
	onVacuum := readPragmas(ctx, vacuumPath, pragmas)
	fmt.Println("set on the original with sqlitex.Open, then closed; each file reopened with a plain driver connection (no settings applied)")
	fmt.Printf("  %-20s %-12s %-12s %-12s %-12s %s\n", "setting", "original", "default", "copy", "VACUUM INTO", "lives in")
	var table []observed
	allAsDeclared := true
	for _, c := range cases {
		home := sqlitex.SettingPerConnection
		if onCopy[c.pragma] == c.set {
			home = sqlitex.SettingInFile
		}
		placement, ok := sqlitex.SettingPlacementFor(c.name)
		if !ok {
			fatal("placement", fmt.Errorf("no declared placement for %s", c.name))
		}
		if placement.Home != home || onOriginal[c.name] != c.set || defaults[c.pragma] == c.set {
			allAsDeclared = false
		}
		fmt.Printf("  %-20s %-12s %-12s %-12s %-12s %s\n", c.name, onOriginal[c.name], defaults[c.pragma], onCopy[c.pragma], onVacuum[c.pragma], home)
		table = append(table, observed{Setting: c.name, Home: home, Set: onOriginal[c.name], Default: defaults[c.pragma], Copy: onCopy[c.pragma], VacuumInto: onVacuum[c.pragma]})
	}
	check(allAsDeclared, "every value set differs from the fresh default, and every setting that survived the copy is exactly a setting sqlitex.SettingPlacements declares as living in the file")
	inFileKept := true
	for _, row := range table {
		if row.Home == sqlitex.SettingInFile && row.Setting != "journal_mode=wal" && row.VacuumInto != row.Set {
			inFileKept = false
		}
	}
	check(inFileKept && onVacuum["journal_mode"] == sqlitex.JournalDelete, "VACUUM INTO keeps every file setting except journal_mode: its output is a rollback-mode file (header bytes 18-19 = 1)")

	fmt.Println("\nthe two journal_mode cases and the deprecated default_cache_size:")
	persistPath := filepath.Join(dir, "persist.db")
	pdb, pconn := plainConnection(ctx, persistPath)
	if mode := readPragma(ctx, pconn, "journal_mode=persist"); mode != sqlitex.JournalPersist {
		fatal("persist", fmt.Errorf("got %s", mode))
	}
	if _, err := pconn.ExecContext(ctx, "CREATE TABLE t(x)"); err != nil {
		fatal("create", err)
	}
	closePlain(pdb, pconn)
	persistReopened := readPragmas(ctx, persistPath, []string{"journal_mode"})["journal_mode"]
	fmt.Printf("  journal_mode=persist set on a new file, reopened: %s\n", persistReopened)
	cachePath := filepath.Join(dir, "default-cache-size.db")
	cdb, cconn := plainConnection(ctx, cachePath)
	if _, err := cconn.ExecContext(ctx, "PRAGMA default_cache_size=777; CREATE TABLE t(x)"); err != nil {
		fatal("default_cache_size", err)
	}
	closePlain(cdb, cconn)
	cacheCopy := filepath.Join(dir, "default-cache-size-copy.db")
	copyFile(cachePath, cacheCopy)
	reopened := readPragmas(ctx, cacheCopy, []string{"default_cache_size", "cache_size"})
	header, err := os.ReadFile(cacheCopy)
	if err != nil || len(header) < 100 {
		fatal("read header", fmt.Errorf("%d bytes, %v", len(header), err))
	}
	stored := int32(uint32(header[48])<<24 | uint32(header[49])<<16 | uint32(header[50])<<8 | uint32(header[51]))
	fmt.Printf("  PRAGMA default_cache_size=777, then a copy opened: default_cache_size %s, cache_size %s, header bytes 48-51 = %d\n", reopened["default_cache_size"], reopened["cache_size"], stored)
	check(persistReopened == sqlitex.JournalDelete, "a rollback journal mode (persist) does not survive a reopen: it is per connection; only wal is stored in the file")
	check(reopened["cache_size"] == "777" && stored == 777, "the deprecated PRAGMA default_cache_size is stored in header bytes 48-51 and becomes the starting cache_size of every new connection to the file")

	document := struct {
		Placements []sqlitex.SettingPlacement `json:"placements"`
		Observed   []observed                 `json:"observed"`
	}{sqlitex.SettingPlacements(), table}
	encoded, err := json.Marshal(document)
	if err != nil {
		fatal("json", err)
	}
	fmt.Printf("settings-json: %s\n", encoded)
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child-open" {
		childOpen(os.Args[2])
		return
	}
	ctx := context.Background()
	sqlitex.MustPinCheckMemory(ctx)
	work := sqlitex.MustWorkDir("8.1")
	partA(ctx, work)
	checkSchemaMemory()
	partB(ctx, work)
	if len(failures) > 0 {
		fmt.Printf("\n%d check(s) FAILED\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("\nall checks passed")
}
