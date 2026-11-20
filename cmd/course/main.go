package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const usage = `usage: go run ./cmd/course <command> [arguments]

commands:
  fetch-source                         clone the pinned SQLite tree into _src/ and verify it
  build-tools [-reuse]                 build sqlite3, showwal, dbhash from a copy of the pinned tree into _tools/bin/
  inline [-check] <chapter.md>...      rewrite (or with -check, verify) code blocks under include markers
  check sources [chNN]                 verify SOURCES.sha256 against the pinned tree and the chapter copies
  check spans [chNN]                   verify SPANS.txt symbols start at the cited lines of the pinned tree
  check comments <old-rev> <new-rev>   verify Go code is unchanged apart from comments
  check prose <base>..<head>           verify every comment and chapter line was written by the prose author
  check evidence [chNN] [-runs N]      run each experiment N times and compare with its evidence file
  check gofmt                          verify tracked Go files are gofmt-formatted
  check paths                          verify no tracked file mentions a machine-local path
  check all [-runs N] [-skip-evidence] every check that needs no revisions
`

type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	root, err := sqlitex.RepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(2)
	}
	command, args := os.Args[1], os.Args[2:]
	switch command {
	case "fetch-source":
		err = runFetchSource(ctx, root, args)
	case "build-tools":
		err = runBuildTools(ctx, root, args)
	case "inline":
		err = runInline(root, args)
	case "check":
		err = runCheck(ctx, root, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		os.Exit(2)
	}
	if err != nil {
		if exit, ok := err.(exitError); ok {
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func usageError(format string, args ...any) error {
	fmt.Fprintf(os.Stderr, format+"\n\n%s", append(args, usage)...)
	return exitError{2}
}

func joinArgs(args []string) string {
	return strings.Join(args, " ")
}
