package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
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

// buildScratchRepo clones only the Go packages needed to compile and run the
// Chapter 1 experiments under _work/adv-ch01/pin-falsify so we can mutate
// sqlite/PIN without ever touching the repository checkout.
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
		"ch01-architecture-and-execution-path/experiments",
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

func writeScript(path, body string) error {
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func main() {
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch01", "pin-falsify")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}
	workDir := filepath.Join(scratch, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir work", err)
	}
	envWork := []string{"COURSE_WORK_DIR=" + workDir}

	// Locate the real pinned CLI up front so wrapper attacks can delegate to it.
	realCLI, err := sqlitex.LocateCLI(ctx)
	if err != nil {
		fatal("locate real CLI", err)
	}

	// Build a scratch repo with a mutated fossil_id / source_id in sqlite/PIN
	// to verify that all three Chapter 1 experiments refuse to run on a mismatched build.
	scratchRepo := filepath.Join(scratch, "scratch-repo")
	if err := buildScratchRepo(repoRoot, scratchRepo); err != nil {
		fatal("build scratch repo", err)
	}
	pinPath := filepath.Join(scratchRepo, "sqlite", "PIN")
	origPINBytes, err := os.ReadFile(pinPath)
	if err != nil {
		fatal("read PIN", err)
	}
	pin := sqlite.Current()
	badFossil := pin.FossilID[:len(pin.FossilID)-1] + "d"
	if strings.HasSuffix(pin.FossilID, "d") {
		badFossil = pin.FossilID[:len(pin.FossilID)-1] + "e"
	}
	mutatedPIN := strings.ReplaceAll(string(origPINBytes), pin.FossilID, badFossil)
	if err := os.WriteFile(pinPath, []byte(mutatedPIN), 0o644); err != nil {
		fatal("write mutated PIN", err)
	}

	experiments := []struct {
		id      string
		pkg     string
		okToken string
	}{
		{"PIN-01", "./ch01-architecture-and-execution-path/experiments/1.1-pin-check", "PIN CHECK OK"},
		{"PIN-02", "./ch01-architecture-and-execution-path/experiments/1.2-one-insert", "ONE INSERT OK"},
		{"PIN-03", "./ch01-architecture-and-execution-path/experiments/1.3-recipe-rebuild", "RECIPE REBUILD OK"},
	}
	for _, exp := range experiments {
		res, err := runGoPkg(ctx, scratchRepo, exp.pkg, envWork)
		if err != nil {
			fatal("run "+exp.id, err)
		}
		line1 := firstLine(res.stdout)
		held := res.exitCode != 0 &&
			strings.HasPrefix(line1, "sqlite_source_id:") &&
			strings.Contains(line1, "NOT the pin") &&
			!strings.Contains(res.stdout, exp.okToken)
		report(
			exp.id,
			fmt.Sprintf("run %s against scratch repo with mutated sqlite/PIN fossil_id", filepath.Base(exp.pkg)),
			"non-zero exit and line 1 reports sqlite_source_id NOT the pin",
			fmt.Sprintf("exit=%d line1=%q", res.exitCode, line1),
			held,
		)
	}

	// PIN-04: Keep source_id matching, but mutate driver_version in sqlite/PIN.
	// Experiment 1.1 checks debug.ReadBuildInfo() against PIN.DriverVersion.
	badDriverPIN := strings.Replace(string(origPINBytes), "driver_version="+pin.DriverVersion, "driver_version=v1.99.9", 1)
	if err := os.WriteFile(pinPath, []byte(badDriverPIN), 0o644); err != nil {
		fatal("write bad driver PIN", err)
	}
	res04, err := runGoPkg(ctx, scratchRepo, "./ch01-architecture-and-execution-path/experiments/1.1-pin-check", envWork)
	if err != nil {
		fatal("run PIN-04", err)
	}
	held04 := res04.exitCode != 0 &&
		strings.Contains(res04.stdout, "check FAILED: the linked driver is") &&
		strings.Contains(res04.stdout, "PIN CHECK FAILED")
	report(
		"PIN-04",
		"run 1.1-pin-check with matching source_id but mismatched driver_version=v1.99.9 in scratch sqlite/PIN",
		"non-zero exit and linked driver version check FAILED",
		fmt.Sprintf("exit=%d hasFailedCheck=%v", res04.exitCode, strings.Contains(res04.stdout, "check FAILED: the linked driver is")),
		held04,
	)

	// PIN-05: Fake CLI via SQLITE3_BIN that prints a wrong sqlite_source_id().
	// Both 1.1 and 1.2 call LocateCLI and must reject the mismatched CLI.
	wrongCLIScript := filepath.Join(scratch, "fake-cli-wrong-pin.sh")
	if err := writeScript(wrongCLIScript, "#!/bin/sh\necho '2026-07-24 19:02:57 0000000000000000000000000000000000000000000000000000000000000000'\n"); err != nil {
		fatal("write wrongCLI", err)
	}
	envWrongCLI := append(append([]string(nil), envWork...), "SQLITE3_BIN="+wrongCLIScript)
	r11, err := runGoPkg(ctx, repoRoot, "./ch01-architecture-and-execution-path/experiments/1.1-pin-check", envWrongCLI)
	if err != nil {
		fatal("run PIN-05 1.1", err)
	}
	r12, err := runGoPkg(ctx, repoRoot, "./ch01-architecture-and-execution-path/experiments/1.2-one-insert", envWrongCLI)
	if err != nil {
		fatal("run PIN-05 1.2", err)
	}
	held05 := r11.exitCode != 0 && r12.exitCode != 0 &&
		strings.Contains(r11.stdout, "PIN CHECK FAILED") &&
		strings.Contains(r12.stdout, "ONE INSERT FAILED")
	report(
		"PIN-05",
		"run 1.1 and 1.2 with SQLITE3_BIN returning a wrong sqlite_source_id()",
		"both 1.1 and 1.2 exit non-zero and report FAILED",
		fmt.Sprintf("1.1 exit=%d failed=%v; 1.2 exit=%d failed=%v",
			r11.exitCode, strings.Contains(r11.stdout, "PIN CHECK FAILED"),
			r12.exitCode, strings.Contains(r12.stdout, "ONE INSERT FAILED")),
		held05,
	)

	// PIN-06: Fake CLI that delegates to the real CLI (passing source_id and version)
	// but strips ENABLE_DBPAGE_VTAB from compile_options output.
	dropOptScript := filepath.Join(scratch, "fake-cli-drop-dbpage.sh")
	body06 := fmt.Sprintf("#!/bin/sh\n'%s' \"$@\" | grep -vx 'ENABLE_DBPAGE_VTAB'\n", realCLI.Path)
	if err := writeScript(dropOptScript, body06); err != nil {
		fatal("write dropOpt", err)
	}
	r06, err := runGoPkg(ctx, repoRoot, "./ch01-architecture-and-execution-path/experiments/1.1-pin-check",
		append(append([]string(nil), envWork...), "SQLITE3_BIN="+dropOptScript))
	if err != nil {
		fatal("run PIN-06", err)
	}
	held06 := r06.exitCode != 0 &&
		strings.Contains(r06.stdout, "check FAILED: the CLI is compiled with ENABLE_DBPAGE_VTAB")
	report(
		"PIN-06",
		"run 1.1 with fake CLI matching source_id/version but missing ENABLE_DBPAGE_VTAB",
		"non-zero exit detecting missing required CLI compile option",
		fmt.Sprintf("exit=%d detected=%v", r06.exitCode, strings.Contains(r06.stdout, "check FAILED: the CLI is compiled with ENABLE_DBPAGE_VTAB")),
		held06,
	)

	// PIN-07: Fake CLI with all required options intact, but DEFAULT_PAGE_SIZE=8192
	// instead of 4096 to verify the file-format/I/O option table catches drift.
	driftOptScript := filepath.Join(scratch, "fake-cli-drift-pagesize.sh")
	body07 := fmt.Sprintf("#!/bin/sh\n'%s' \"$@\" | sed 's/^DEFAULT_PAGE_SIZE=4096$/DEFAULT_PAGE_SIZE=8192/'\n", realCLI.Path)
	if err := writeScript(driftOptScript, body07); err != nil {
		fatal("write driftOpt", err)
	}
	r07, err := runGoPkg(ctx, repoRoot, "./ch01-architecture-and-execution-path/experiments/1.1-pin-check",
		append(append([]string(nil), envWork...), "SQLITE3_BIN="+driftOptScript))
	if err != nil {
		fatal("run PIN-07", err)
	}
	held07 := r07.exitCode != 0 &&
		strings.Contains(r07.stdout, "DEFAULT_PAGE_SIZE") &&
		strings.Contains(r07.stdout, "DIFFERENT") &&
		strings.Contains(r07.stdout, "PIN CHECK FAILED")
	report(
		"PIN-07",
		"run 1.1 with fake CLI reporting DEFAULT_PAGE_SIZE=8192 instead of 4096",
		"non-zero exit marking DEFAULT_PAGE_SIZE DIFFERENT",
		fmt.Sprintf("exit=%d markedDifferent=%v", r07.exitCode, strings.Contains(r07.stdout, "DIFFERENT")),
		held07,
	)

	// PIN-08: Verify raw modernc.org/sqlite runtime defaults on a real WAL database
	// without sqlitex setting any PRAGMAs (checks OUTLINE §4.1 claim that modernc
	// preserves upstream defaults: synchronous=FULL(2), busy_timeout=0, page_size=4096).
	rawDBPath := filepath.Join(scratch, "raw-defaults.db")
	db1, err := sql.Open("sqlite", rawDBPath)
	if err != nil {
		fatal("raw open 1", err)
	}
	if _, err := db1.ExecContext(ctx, "PRAGMA journal_mode=wal; CREATE TABLE x(id INTEGER PRIMARY KEY)"); err != nil {
		fatal("raw init", err)
	}
	if err := db1.Close(); err != nil {
		fatal("raw close 1", err)
	}
	db2, err := sql.Open("sqlite", rawDBPath)
	if err != nil {
		fatal("raw open 2", err)
	}
	defer db2.Close()
	var syncMode, busyTimeout, foreignKeys, pageSize int
	var journalMode string
	if err := db2.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		fatal("raw pragma journal_mode", err)
	}
	if err := db2.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode); err != nil {
		fatal("raw pragma synchronous", err)
	}
	if err := db2.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		fatal("raw pragma busy_timeout", err)
	}
	if err := db2.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		fatal("raw pragma foreign_keys", err)
	}
	if err := db2.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		fatal("raw pragma page_size", err)
	}
	held08 := journalMode == "wal" && syncMode == 2 && busyTimeout == 0 && foreignKeys == 0 && pageSize == 4096
	report(
		"PIN-08",
		"open WAL database via raw sql.Open(\"sqlite\", ...) with no PRAGMAs",
		"upstream defaults preserved: journal_mode=wal synchronous=2(FULL) busy_timeout=0 foreign_keys=0 page_size=4096",
		fmt.Sprintf("journal_mode=%s synchronous=%d busy_timeout=%d foreign_keys=%d page_size=%d", journalMode, syncMode, busyTimeout, foreignKeys, pageSize),
		held08,
	)
}
