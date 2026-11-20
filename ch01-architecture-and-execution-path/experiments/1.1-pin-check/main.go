// Experiment 1.1 verifies that both SQLite builds used by the course—the
// modernc.org/sqlite Go driver and the pinned sqlite3 C CLI—run the pinned
// SQLite 3.53.4 check-in and share the required compile-time options.
//
// Run from the repo root:
//
//	go run ./ch01-architecture-and-execution-path/experiments/1.1-pin-check
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
)

// requiredOptions lists the compile options later labs depend on: sqlite_dbpage
// and dbstat virtual tables for page inspection, and upstream's FULL sync
// default in WAL mode (DEFAULT_WAL_SYNCHRONOUS=2).
var requiredOptions = []string{
	"ENABLE_DBPAGE_VTAB",
	"ENABLE_DBSTAT_VTAB",
	"DEFAULT_WAL_SYNCHRONOUS=2",
}

// fileFormatAndIOOptions lists compile options that govern on-disk format,
// cache sizing, mmap limits, or sync defaults; both builds must agree on all
// of them.
var fileFormatAndIOOptions = []string{
	"DEFAULT_AUTOVACUUM",
	"DEFAULT_CACHE_SIZE",
	"DEFAULT_FILE_FORMAT",
	"DEFAULT_JOURNAL_SIZE_LIMIT",
	"DEFAULT_MMAP_SIZE",
	"DEFAULT_PAGE_SIZE",
	"DEFAULT_PCACHE_INITSZ",
	"DEFAULT_SECTOR_SIZE",
	"DEFAULT_SYNCHRONOUS",
	"DEFAULT_WAL_AUTOCHECKPOINT",
	"DEFAULT_WAL_SYNCHRONOUS",
	"DIRECT_OVERFLOW_READ",
	"MAX_MMAP_SIZE",
	"MAX_PAGE_SIZE",
	"TEMP_STORE",
}

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

func optionName(option string) string {
	name, _, _ := strings.Cut(option, "=")
	return name
}

func findOption(options []string, name string) (string, bool) {
	for _, option := range options {
		if optionName(option) == name {
			return option, true
		}
	}
	return "", false
}

func driverOptions(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var options []string
	for rows.Next() {
		var option string
		if err := rows.Scan(&option); err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, rows.Err()
}

// moduleVersion reads the linked Go module version from the binary's build
// info rather than go.mod so a replace directive or upgrade is caught at run
// time.
func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(no build info)"
	}
	for _, dep := range info.Deps {
		if dep.Path == path {
			if dep.Replace != nil {
				return dep.Version + " replaced by " + dep.Replace.Path + " " + dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "(not linked)"
}

func main() {
	ctx := context.Background()
	pin := sqlite.Current()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		fatal("open :memory:", err)
	}
	defer db.Close()
	// Every course experiment prints sqlite_source_id() first and exits
	// non-zero if the linked build does not match sqlite/PIN.
	sqlitex.MustPinCheck(ctx, db)

	fmt.Printf("pin: release %s, tag %s, git %s, fossil %s\n", pin.Release, pin.Tag, pin.GitCommit, pin.FossilID)
	driverVersion := moduleVersion(pin.DriverModule)
	libcVersion := moduleVersion(pin.LibcModule)
	fmt.Printf("driver module linked: %s %s\n", pin.DriverModule, driverVersion)
	fmt.Printf("libc module linked:   %s %s\n", pin.LibcModule, libcVersion)
	check(driverVersion == pin.DriverVersion, fmt.Sprintf("the linked driver is %s %s", pin.DriverModule, pin.DriverVersion))
	check(libcVersion == pin.LibcVersion, fmt.Sprintf("the linked libc is %s %s", pin.LibcModule, pin.LibcVersion))

	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		fatal("sqlite_version", err)
	}
	fmt.Printf("driver sqlite_version(): %s\n", version)
	check(version == pin.Release, "the driver reports release "+pin.Release)

	options, err := driverOptions(ctx, db)
	if err != nil {
		fatal("compile_options", err)
	}
	fmt.Printf("driver compile options (%d):\n", len(options))
	for _, option := range options {
		fmt.Printf("  %s\n", option)
	}
	for _, required := range requiredOptions {
		check(slices.Contains(options, required), "the driver is compiled with "+required)
	}

	// Smoke-test both page-inspection virtual tables on a 2-page database
	// (page 1 for sqlite_schema, page 2 for table t).
	var dbpagePages, dbstatRows int
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t VALUES(1, 'one')"); err != nil {
		fatal("create", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_dbpage").Scan(&dbpagePages); err != nil {
		fatal("sqlite_dbpage", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM dbstat").Scan(&dbstatRows); err != nil {
		fatal("dbstat", err)
	}
	fmt.Printf("driver sqlite_dbpage rows: %d; dbstat rows: %d\n", dbpagePages, dbstatRows)
	check(dbpagePages == 2 && dbstatRows == 2, "sqlite_dbpage and dbstat answer queries in the driver (2 pages: schema and table t)")

	cli, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fmt.Printf("sqlite3 CLI: %v\n", err)
		failures = append(failures, "the pinned sqlite3 CLI is available")
		finish()
		return
	}
	fmt.Println(cli.PinLine())
	fmt.Printf("INFO: CLI %s (%s)\n", displayPath(cli.Path), cli.Origin)
	out, err := cli.Exec(ctx, ":memory:", "SELECT sqlite_version();\nPRAGMA compile_options;\n")
	if err != nil {
		fatal("CLI compile_options", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	cliVersion, cliOptions := lines[0], lines[1:]
	fmt.Printf("CLI sqlite_version(): %s\n", cliVersion)
	check(cliVersion == pin.Release, "the CLI reports release "+pin.Release)
	for _, required := range requiredOptions {
		check(slices.Contains(cliOptions, required), "the CLI is compiled with "+required)
	}

	fmt.Println("file-format and I/O options, driver | CLI:")
	same := true
	for _, name := range fileFormatAndIOOptions {
		driverValue, inDriver := findOption(options, name)
		cliValue, inCLI := findOption(cliOptions, name)
		if !inDriver {
			driverValue = "(absent)"
		}
		if !inCLI {
			cliValue = "(absent)"
		}
		mark := "same"
		if driverValue != cliValue {
			mark = "DIFFERENT"
			same = false
		}
		fmt.Printf("  %-27s %-32s | %-32s %s\n", name, driverValue, cliValue, mark)
	}
	check(same, "the driver and the CLI agree on every file-format and I/O compile option listed")
	var onlyDriver, onlyCLI []string
	for _, option := range options {
		if !slices.Contains(cliOptions, option) {
			onlyDriver = append(onlyDriver, option)
		}
	}
	for _, option := range cliOptions {
		if !slices.Contains(options, option) {
			onlyCLI = append(onlyCLI, option)
		}
	}
	// Report build-specific differences on INFO lines so EXACT evidence stays
	// portable between the system CLI and the ./configure build-tools CLI.
	fmt.Printf("INFO: compile options only in the driver: %s\n", strings.Join(onlyDriver, " "))
	fmt.Printf("INFO: compile options only in this CLI: %s\n", strings.Join(onlyCLI, " "))
	finish()
}

func finish() {
	if len(failures) > 0 {
		fmt.Printf("PIN CHECK FAILED: %d claim(s) do not hold: %s\n", len(failures), strings.Join(failures, "; "))
		os.Exit(1)
	}
	fmt.Println("PIN CHECK OK")
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
