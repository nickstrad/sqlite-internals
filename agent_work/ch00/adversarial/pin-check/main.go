// Command pin-check runs adversarial attacks against sqlite/PIN and
// internal/sqlitex's runtime pin checks (Go driver and sqlite3 CLI).
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/sqlite"
)

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ; ")
	return strings.TrimSpace(s)
}

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n",
		id, sanitize(tried), sanitize(expect), sanitize(got), verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "HARNESS ERROR: %s: %v\n", msg, err)
	os.Exit(2)
}

func copyDirNoTests(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// runScratchDriverPin builds a throwaway copy of this module under scratchDir
// pinned to driverVersion and runs sqlitex.MustPinCheckMemory against it.
func runScratchDriverPin(repoRoot, scratchDir, driverVersion string) (exitCode int, stdout, stderr string, err error) {
	if err := os.RemoveAll(scratchDir); err != nil {
		return 0, "", "", err
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		return 0, "", "", err
	}
	if err := copyDirNoTests(filepath.Join(repoRoot, "sqlite"), filepath.Join(scratchDir, "sqlite")); err != nil {
		return 0, "", "", err
	}
	if err := copyDirNoTests(filepath.Join(repoRoot, "internal", "sqlitex"), filepath.Join(scratchDir, "internal", "sqlitex")); err != nil {
		return 0, "", "", err
	}
	goMod := fmt.Sprintf("module github.com/nickstrad/walfleet\n\ngo 1.26.0\n\nrequire modernc.org/sqlite %s\n", driverVersion)
	if err := os.WriteFile(filepath.Join(scratchDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		return 0, "", "", err
	}
	probeMain := `package main

import (
	"context"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

func main() {
	sqlitex.MustPinCheckMemory(context.Background())
}
`
	if err := os.WriteFile(filepath.Join(scratchDir, "main.go"), []byte(probeMain), 0o644); err != nil {
		return 0, "", "", err
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = scratchDir
	if out, tidyErr := tidy.CombinedOutput(); tidyErr != nil {
		return 0, "", "", fmt.Errorf("go mod tidy (%s): %w: %s", driverVersion, tidyErr, strings.TrimSpace(string(out)))
	}

	binPath := filepath.Join(scratchDir, "probe")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, ".")
	build.Dir = scratchDir
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		return 0, "", "", fmt.Errorf("go build (%s): %w: %s", driverVersion, buildErr, strings.TrimSpace(string(out)))
	}

	run := exec.Command(binPath)
	run.Dir = scratchDir
	var outBuf, errBuf bytes.Buffer
	run.Stdout = &outBuf
	run.Stderr = &errBuf
	runErr := run.Run()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), outBuf.String(), errBuf.String(), nil
		}
		return 0, outBuf.String(), errBuf.String(), runErr
	}
	return 0, outBuf.String(), errBuf.String(), nil
}

func writeFakeCLI(path, sourceID string) error {
	script := fmt.Sprintf("#!/bin/sh\necho '%s'\n", sourceID)
	return os.WriteFile(path, []byte(script), 0o755)
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-ch00", "pin-check")
	if err := os.RemoveAll(workDir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("create work dir", err)
	}

	// PIN-01: scratch module pinned to a newer modernc.org/sqlite release (different SQLite version).
	code, stdout, stderr, err := runScratchDriverPin(repoRoot, filepath.Join(workDir, "mod-v1.62.0"), "v1.62.0")
	if err != nil {
		fatal("PIN-01 scratch module v1.62.0", err)
	}
	held01 := code != 0 &&
		strings.Contains(stdout, "NOT the pin 3.53.4") &&
		strings.Contains(stderr, "FAIL:") &&
		strings.Contains(stderr, sqlite.Current().SourceID)
	report(
		"PIN-01",
		"scratch module pinned to modernc.org/sqlite v1.62.0 (different SQLite release) running sqlitex.MustPinCheckMemory",
		"non-zero exit with loud pin mismatch message on stdout and stderr",
		fmt.Sprintf("exit=%d stdout=%q stderr=%q", code, strings.TrimSpace(stdout), strings.TrimSpace(stderr)),
		held01,
	)

	// PIN-02: scratch module pinned to an older modernc.org/sqlite release (v1.58.0) that still
	// wraps SQLite 3.53.4. Tests whether sqlitex's runtime pin check detects a driver release drift
	// when sqlite_source_id() alone still matches 3.53.4.
	code, stdout, stderr, err = runScratchDriverPin(repoRoot, filepath.Join(workDir, "mod-v1.58.0"), "v1.58.0")
	if err != nil {
		fatal("PIN-02 scratch module v1.58.0", err)
	}
	held02 := code != 0 && strings.Contains(stderr, "FAIL:")
	report(
		"PIN-02",
		"scratch module pinned to modernc.org/sqlite v1.58.0 (wrong driver release, same SQLite 3.53.4) running sqlitex.MustPinCheckMemory",
		"non-zero exit catching driver module version v1.58.0 != v1.60.1",
		fmt.Sprintf("exit=%d stdout=%q stderr=%q", code, strings.TrimSpace(stdout), strings.TrimSpace(stderr)),
		held02,
	)

	// PIN-03: $SQLITE3_BIN points to a fake CLI reporting a non-pinned source id.
	if runtime.GOOS != "windows" {
		fakeEnvCLI := filepath.Join(workDir, "fake-env-sqlite3")
		if err := writeFakeCLI(fakeEnvCLI, "2026-01-01 00:00:00 deadbeefdeadbeef"); err != nil {
			fatal("write fake env CLI", err)
		}
		origBin, hadBin := os.LookupEnv(sqlitex.CLIEnv)
		os.Setenv(sqlitex.CLIEnv, fakeEnvCLI)
		_, locateErr := sqlitex.LocateCLI(context.Background())
		if hadBin {
			os.Setenv(sqlitex.CLIEnv, origBin)
		} else {
			os.Unsetenv(sqlitex.CLIEnv)
		}
		var mismatch *sqlitex.PinMismatchError
		held03 := errors.As(locateErr, &mismatch)
		report(
			"PIN-03",
			"$SQLITE3_BIN set to fake CLI returning wrong sqlite_source_id()",
			"LocateCLI fails with PinMismatchError without falling back to _tools or PATH",
			fmt.Sprintf("err=%v isPinMismatch=%v", locateErr, held03),
			held03,
		)

		// PIN-04: $COURSE_TOOLS_DIR/bin/sqlite3 reports a wrong source id while real CLI exists on PATH.
		fakeToolsDir := filepath.Join(workDir, "fake-tools")
		if err := os.MkdirAll(filepath.Join(fakeToolsDir, "bin"), 0o755); err != nil {
			fatal("mkdir fake tools", err)
		}
		fakeToolsCLI := filepath.Join(fakeToolsDir, "bin", "sqlite3")
		if err := writeFakeCLI(fakeToolsCLI, "2026-01-01 00:00:00 cafebabecafebabe"); err != nil {
			fatal("write fake tools CLI", err)
		}
		origTools, hadTools := os.LookupEnv(sqlitex.ToolsDirEnv)
		origBin2, hadBin2 := os.LookupEnv(sqlitex.CLIEnv)
		os.Unsetenv(sqlitex.CLIEnv)
		os.Setenv(sqlitex.ToolsDirEnv, fakeToolsDir)
		_, toolsErr := sqlitex.LocateCLI(context.Background())
		if hadTools {
			os.Setenv(sqlitex.ToolsDirEnv, origTools)
		} else {
			os.Unsetenv(sqlitex.ToolsDirEnv)
		}
		if hadBin2 {
			os.Setenv(sqlitex.CLIEnv, origBin2)
		}
		var toolsMismatch *sqlitex.PinMismatchError
		held04 := errors.As(toolsErr, &toolsMismatch)
		report(
			"PIN-04",
			"$COURSE_TOOLS_DIR/bin/sqlite3 reports wrong sqlite_source_id() while pinned sqlite3 exists on PATH",
			"LocateCLI fails with PinMismatchError and refuses to fall back to PATH",
			fmt.Sprintf("err=%v isPinMismatch=%v", toolsErr, held04),
			held04,
		)
	}

	// PIN-05: sqlite.ParsePin rejects tampered PIN files (mismatched fossil/source id, missing key, duplicate key).
	rawPin, err := os.ReadFile(filepath.Join(repoRoot, "sqlite", "PIN"))
	if err != nil {
		fatal("read sqlite/PIN", err)
	}
	tamperedCases := []struct {
		name string
		text string
	}{
		{"mismatched-fossil-id", strings.Replace(string(rawPin), "fossil_id=bf7c", "fossil_id=0000", 1)},
		{"missing-driver-version", strings.Replace(string(rawPin), "driver_version=v1.60.1\n", "", 1)},
		{"duplicate-release", string(rawPin) + "\nrelease=3.53.4\n"},
	}
	var accepted []string
	for _, tc := range tamperedCases {
		if _, parseErr := sqlite.ParsePin(tc.text); parseErr == nil {
			accepted = append(accepted, tc.name)
		}
	}
	held05 := len(accepted) == 0
	report(
		"PIN-05",
		"sqlite.ParsePin given mismatched fossil_id, missing driver_version, and duplicate release key",
		"all three malformed PIN variants rejected with errors",
		fmt.Sprintf("accepted=%v (rejected %d/3)", accepted, 3-len(accepted)),
		held05,
	)
}
