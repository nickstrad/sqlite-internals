package sqlitex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CLIEnv names the environment variable that selects an explicit sqlite3 CLI binary.
const CLIEnv = "SQLITE3_BIN"

// ToolsDirEnv names the environment variable that overrides the default _tools/
// directory where cmd/course build-tools installs sqlite3, showwal, and dbhash.
const ToolsDirEnv = "COURSE_TOOLS_DIR"

// ToolsDir returns the directory holding compiled SQLite tools:
// $COURSE_TOOLS_DIR if set, otherwise <repo>/_tools.
func ToolsDir() (string, error) {
	if dir := os.Getenv(ToolsDirEnv); dir != "" {
		return filepath.Abs(dir)
	}
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "_tools"), nil
}

// CLI represents a verified sqlite3 command-line binary built from the pinned
// SQLite check-in.
type CLI struct {
	Path     string
	SourceID string
	Origin   string
}

// CLIResult holds the captured streams and exit status of one CLI invocation.
type CLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// CLIRun specifies arguments, stdin, working directory, and environment for
// running the sqlite3 CLI.
type CLIRun struct {
	Args  []string
	Stdin io.Reader
	Dir   string
	Env   []string
}

func toolCandidates(name, envVar string) ([]string, []string) {
	var paths, origins []string
	if envVar != "" {
		if value := os.Getenv(envVar); value != "" {
			paths = append(paths, value)
			origins = append(origins, "$"+envVar)
		}
	}
	if tools, err := ToolsDir(); err == nil {
		paths = append(paths, filepath.Join(tools, "bin", name))
		origins = append(origins, "tools dir")
	}
	if found, err := exec.LookPath(name); err == nil {
		paths = append(paths, found)
		origins = append(origins, "PATH")
	}
	return paths, origins
}

// LocateTool finds a companion tool built from the pinned tree (such as showwal
// or dbhash) by searching ToolsDir()/bin and then PATH.
func LocateTool(name string) (string, error) {
	paths, _ := toolCandidates(name, "")
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found in $COURSE_TOOLS_DIR/bin, _tools/bin or on PATH; run: go run ./cmd/course build-tools", name)
}

// LocateCLI finds the sqlite3 CLI in order ($SQLITE3_BIN, ToolsDir()/bin, PATH)
// and verifies that the first existing candidate reports the pinned sqlite_source_id().
// An invalid $SQLITE3_BIN or a pin mismatch returns an error instead of falling back.
func LocateCLI(ctx context.Context) (CLI, error) {
	paths, origins := toolCandidates("sqlite3", CLIEnv)
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			if origins[i] == "$"+CLIEnv {
				return CLI{}, fmt.Errorf("$%s=%s does not name a file", CLIEnv, path)
			}
			continue
		}
		cli := CLI{Path: path, Origin: origins[i]}
		id, err := cli.QuerySourceID(ctx)
		if err != nil {
			return CLI{}, fmt.Errorf("sqlite3 CLI %s: %w", path, err)
		}
		cli.SourceID = id
		if err := CheckSourceID("sqlite3 CLI "+path, id); err != nil {
			return cli, err
		}
		return cli, nil
	}
	return CLI{}, errors.New("no sqlite3 CLI found ($SQLITE3_BIN, $COURSE_TOOLS_DIR/bin or _tools/bin, PATH); run: go run ./cmd/course build-tools")
}

// QuerySourceID runs sqlite3 against :memory: and returns SELECT sqlite_source_id().
func (c CLI) QuerySourceID(ctx context.Context) (string, error) {
	result, err := c.Run(ctx, CLIRun{Args: []string{":memory:", "SELECT sqlite_source_id();"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// baseArgs disables interactive prompts, stops on the first SQL error, and
// ignores any user ~/.sqliterc. Callers never combine -bail with -cmd because
// shell.c.in:13536 exits after any SQL -cmd without running later arguments.
func (c CLI) baseArgs() []string {
	return []string{"-batch", "-bail", "-init", os.DevNull}
}

// Command builds an *exec.Cmd for the CLI with baseArgs prepended, suitable for
// wrapping under strace or another runner.
func (c CLI) Command(ctx context.Context, run CLIRun) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Path, append(c.baseArgs(), run.Args...)...)
	cmd.Dir = run.Dir
	cmd.Stdin = run.Stdin
	if run.Env != nil {
		cmd.Env = run.Env
	}
	return cmd
}

// Run executes the CLI and returns its captured stdout, stderr, and exit code.
func (c CLI) Run(ctx context.Context, run CLIRun) (CLIResult, error) {
	cmd := c.Command(ctx, run)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := CLIResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return result, fmt.Errorf("%s %s: %w: %s", c.Path, strings.Join(run.Args, " "), err, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

// OpenScript builds an stdin script that opens dbPath with ".open --normal"
// before running sql. Naming a file on the command line or using plain ".open"
// makes the 3.53.4 shell probe-open it read-only via isDatabaseFile /
// deduceDatabaseType (shell.c.in:4229-4320, 4495, 10222), adding extra syscalls
// and -shm writes that do not belong to the statement.
func OpenScript(dbPath, sql string) string {
	return ".open --normal " + dotCommandQuote(dbPath) + "\n" + sql
}

var dotCommandEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)

func dotCommandQuote(arg string) string {
	return `"` + dotCommandEscaper.Replace(arg) + `"`
}

// Exec opens dbPath via OpenScript on stdin, executes sql, and returns stdout.
func (c CLI) Exec(ctx context.Context, dbPath, sql string) (string, error) {
	result, err := c.Run(ctx, CLIRun{Stdin: strings.NewReader(OpenScript(dbPath, sql))})
	return result.Stdout, err
}

// PinLine formats the CLI's source ID check line for experiment output.
func (c CLI) PinLine() string {
	return strings.Replace(PinLine(c.SourceID), "sqlite_source_id:", "sqlite3 CLI sqlite_source_id:", 1)
}

// MustLocateCLI locates the pinned sqlite3 CLI, prints its PinLine to stdout,
// and exits 1 on failure.
func MustLocateCLI(ctx context.Context) CLI {
	cli, err := LocateCLI(ctx)
	if cli.SourceID != "" {
		fmt.Println(cli.PinLine())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	return cli
}
