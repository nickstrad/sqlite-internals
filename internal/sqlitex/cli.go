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

const CLIEnv = "SQLITE3_BIN"

const ToolsDirEnv = "COURSE_TOOLS_DIR"

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

type CLI struct {
	Path     string
	SourceID string
	Origin   string
}

type CLIResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

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

func LocateTool(name string) (string, error) {
	paths, _ := toolCandidates(name, "")
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found in $COURSE_TOOLS_DIR/bin, _tools/bin or on PATH; run: go run ./cmd/course build-tools", name)
}

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

func (c CLI) QuerySourceID(ctx context.Context) (string, error) {
	result, err := c.Run(ctx, CLIRun{Args: []string{":memory:", "SELECT sqlite_source_id();"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (c CLI) baseArgs() []string {
	return []string{"-batch", "-bail", "-init", os.DevNull}
}

func (c CLI) Command(ctx context.Context, run CLIRun) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Path, append(c.baseArgs(), run.Args...)...)
	cmd.Dir = run.Dir
	cmd.Stdin = run.Stdin
	if run.Env != nil {
		cmd.Env = run.Env
	}
	return cmd
}

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

func (c CLI) Exec(ctx context.Context, dbPath, sql string) (string, error) {
	result, err := c.Run(ctx, CLIRun{Args: []string{dbPath}, Stdin: strings.NewReader(sql)})
	return result.Stdout, err
}

func (c CLI) PinLine() string {
	return strings.Replace(PinLine(c.SourceID), "sqlite_source_id:", "sqlite3 CLI sqlite_source_id:", 1)
}

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
