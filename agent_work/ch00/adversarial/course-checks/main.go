// Command course-checks runs adversarial "must be able to fail" attacks against
// cmd/course (check comments, check prose, inline -check, check sources, check spans)
// using isolated temp git repositories under _work/adv-ch00/course-checks/.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/sqlite"
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

func runCmd(dir string, extraEnv []string, name string, args ...string) (int, string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), out.String(), nil
		}
		return -1, out.String(), err
	}
	return 0, out.String(), nil
}

func mustGit(dir string, extraEnv []string, args ...string) string {
	code, out, err := runCmd(dir, extraEnv, "git", args...)
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("git %s (exit %d): %s", strings.Join(args, " "), code, strings.TrimSpace(out)), err)
	}
	return strings.TrimSpace(out)
}

func writeFile(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("mkdir for "+path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatal("write "+path, err)
	}
}

// initTempRepo initializes a fresh git repo with a root go.mod matching the course
// module path so cmd/course's RepoRoot() stays rooted inside the temp repo.
func initTempRepo(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean temp repo", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir temp repo", err)
	}
	mustGit(dir, nil, "init", "-q", "-b", "main")
	mustGit(dir, nil, "config", "user.name", "Test Bot")
	mustGit(dir, nil, "config", "user.email", "bot@example.invalid")
	writeFile(filepath.Join(dir, "go.mod"), "module "+sqlitex.ModulePath+"\n\ngo 1.26.0\n")
}

func commitAs(dir, authorName, authorEmail, msg string) string {
	mustGit(dir, nil, "add", "-A")
	env := []string{
		"GIT_AUTHOR_NAME=" + authorName,
		"GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName,
		"GIT_COMMITTER_EMAIL=" + authorEmail,
		"GIT_AUTHOR_DATE=2026-11-20T12:00:00Z",
		"GIT_COMMITTER_DATE=2026-11-20T12:00:00Z",
	}
	mustGit(dir, env, "commit", "-q", "-m", msg)
	return mustGit(dir, nil, "rev-parse", "HEAD")
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-ch00", "course-checks")
	if err := os.RemoveAll(workDir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}

	// Build the real cmd/course binary once into _work/adv-ch00/course-checks/course.
	courseBin := filepath.Join(workDir, "course")
	code, out, err := runCmd(repoRoot, nil, "go", "build", "-buildvcs=false", "-o", courseBin, "./cmd/course")
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("build cmd/course (exit %d): %s", code, out), err)
	}

	runCommentsAttacks(courseBin, filepath.Join(workDir, "repo-comments"))
	runProseAttacks(courseBin, filepath.Join(workDir, "repo-prose"))
	runInlineAttacks(courseBin, filepath.Join(workDir, "repo-inline"))
	runSourcesAndSpansAttacks(courseBin, filepath.Join(workDir, "repo-tree"), filepath.Join(workDir, "mock-sqlite-src"))
}

func runCommentsAttacks(courseBin, dir string) {
	initTempRepo(dir)
	srcBase := `//go:build linux

package sample

func clamp(n, limit int) int {
	if n < limit {
		return n
	}
	return limit
}
`
	writeFile(filepath.Join(dir, "sample.go"), srcBase)
	revBase := commitAs(dir, "Claude Opus", "opus@example.invalid", "base code")

	// CHK-01: logic change (< to <=) buried among extensive comment edits.
	srcSneaky := `//go:build linux

// Package sample demonstrates boundary clamping for page counts.
package sample

// clamp ensures n never exceeds limit.
// Multiple comment lines are added here to mimic a real prose pass.
// Another explanation line covering edge behavior when n equals limit.
func clamp(n, limit int) int {
	// Guard the upper bound carefully.
	if n <= limit { // inline comment hiding the single-byte operator change
		/* block comment */
		return n
	}
	// Fallback return value.
	return limit
}
`
	writeFile(filepath.Join(dir, "sample.go"), srcSneaky)
	revSneaky := commitAs(dir, "Gemini 4 Neon", "neon@example.invalid", "comments plus hidden logic change")

	code, out, err := runCmd(dir, nil, courseBin, "check", "comments", revBase, revSneaky)
	if err != nil {
		fatal("CHK-01", err)
	}
	held01 := code != 0 && strings.Contains(out, "FAIL check comments") && strings.Contains(out, "code changed")
	report(
		"CHK-01",
		"course check comments with 1-char logic mutation (< to <=) hidden inside 10 comment lines",
		"non-zero exit and FAIL reporting code change",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held01,
	)

	// CHK-02: pure comment change (line comments, block comments, inline comment between tokens).
	srcPureComments := `//go:build linux

// Package sample documents clamping without altering code tokens.
package sample

/*
clamp returns n when n is strictly below limit, otherwise limit.
*/
func clamp(n, limit int) int {
	// Pure comment line.
	if n /* mid-expression comment */ < limit {
		return n // trailing comment
	}
	return limit
}
`
	writeFile(filepath.Join(dir, "sample.go"), srcPureComments)
	revPure := commitAs(dir, "Gemini 4 Neon", "neon@example.invalid", "pure comments only")

	code, out, err = runCmd(dir, nil, courseBin, "check", "comments", revBase, revPure)
	if err != nil {
		fatal("CHK-02", err)
	}
	held02 := code == 0 && strings.Contains(out, "PASS check comments")
	report(
		"CHK-02",
		"course check comments with pure comment additions (doc, line, multiline, mid-expression /* */)",
		"exit 0 and PASS check comments",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held02,
	)

	// CHK-03: compiler directive mutation (//go:build linux -> //go:build !linux) disguised as comment edit.
	srcDirective := strings.Replace(srcPureComments, "//go:build linux", "//go:build !linux", 1)
	writeFile(filepath.Join(dir, "sample.go"), srcDirective)
	revDirective := commitAs(dir, "Gemini 4 Neon", "neon@example.invalid", "directive tamper")

	code, out, err = runCmd(dir, nil, courseBin, "check", "comments", revBase, revDirective)
	if err != nil {
		fatal("CHK-03", err)
	}
	held03 := code != 0 && strings.Contains(out, "FAIL check comments")
	report(
		"CHK-03",
		"course check comments when //go:build linux is altered to //go:build !linux",
		"non-zero exit and FAIL because build directives affect compilation",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held03,
	)
}

func runProseAttacks(courseBin, dir string) {
	initTempRepo(dir)
	writeFile(filepath.Join(dir, "lib.go"), "package lib\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	revBase := commitAs(dir, "Claude Opus", "opus@example.invalid", "base uncommented code")

	// CHK-04: non-Neon author adds a Go comment.
	writeFile(filepath.Join(dir, "lib.go"), "package lib\n\n// Add sums two integers.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	revNonNeon := commitAs(dir, "Claude Opus", "opus@example.invalid", "non-neon comment")

	code, out, err := runCmd(dir, nil, courseBin, "check", "prose", revBase+".."+revNonNeon)
	if err != nil {
		fatal("CHK-04", err)
	}
	held04 := code != 0 && strings.Contains(out, "FAIL check prose") && strings.Contains(out, "lib.go:3")
	report(
		"CHK-04",
		"course check prose when a non-Neon commit adds a Go comment at lib.go:3",
		"non-zero exit flagging lib.go:3 and non-Neon author",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held04,
	)

	// Reset to base on a fresh branch to test Neon prose passing.
	mustGit(dir, nil, "checkout", "-q", "-b", "neon-branch", revBase)
	writeFile(filepath.Join(dir, "lib.go"), "package lib\n\n/*\nAdd sums two integers.\nIt has no overflow checks.\n*/\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	writeFile(filepath.Join(dir, "ch01-demo", "ch01-demo.md"), "# Chapter 1\n\nProse authored by Neon.\n")
	revNeon := commitAs(dir, "Gemini 4 Neon", "neon@example.invalid", "neon prose and comments")

	code, out, err = runCmd(dir, nil, courseBin, "check", "prose", revBase+".."+revNeon)
	if err != nil {
		fatal("CHK-05", err)
	}
	held05 := code == 0 && strings.Contains(out, "PASS check prose")
	report(
		"CHK-05",
		"course check prose when Gemini 4 Neon authors Go comments and ch01-demo.md",
		"exit 0 and PASS check prose",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held05,
	)

	// CHK-06: non-Neon author edits a single interior line inside Neon's multiline /* ... */ comment.
	writeFile(filepath.Join(dir, "lib.go"), "package lib\n\n/*\nAdd sums two integers.\nSneaky edit by Opus on an interior block-comment line.\n*/\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	revInterior := commitAs(dir, "Claude Opus", "opus@example.invalid", "edit inside multiline comment")

	code, out, err = runCmd(dir, nil, courseBin, "check", "prose", revBase+".."+revInterior)
	if err != nil {
		fatal("CHK-06", err)
	}
	held06 := code != 0 && strings.Contains(out, "FAIL check prose") && strings.Contains(out, "lib.go:5")
	report(
		"CHK-06",
		"course check prose when non-Neon edits line 2 inside a Neon multiline /* */ comment",
		"non-zero exit flagging lib.go:5",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held06,
	)

	// CHK-07: non-Neon author writes //note:unauthorized prose (no space after //, lowercase word:word).
	// Tests whether IsDirective's permissive regex exempts arbitrary //word:word comments from check prose.
	mustGit(dir, nil, "checkout", "-q", "-b", "directive-bypass", revBase)
	writeFile(filepath.Join(dir, "lib.go"), "package lib\n\n//note:unauthorized prose disguised with a colon\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	revBypass := commitAs(dir, "Claude Opus", "opus@example.invalid", "colon comment by opus")

	code, out, err = runCmd(dir, nil, courseBin, "check", "prose", revBase+".."+revBypass)
	if err != nil {
		fatal("CHK-07", err)
	}
	held07 := code != 0 && strings.Contains(out, "FAIL check prose")
	report(
		"CHK-07",
		"course check prose when non-Neon adds //note:unauthorized prose (non-directive colon comment)",
		"non-zero exit flagging non-Neon comment (not a real Go compiler directive)",
		fmt.Sprintf("exit=%d out=%q", code, strings.TrimSpace(out)),
		held07,
	)
}

func runInlineAttacks(courseBin, dir string) {
	initTempRepo(dir)
	expSrc := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(1)\n}\n"
	writeFile(filepath.Join(dir, "ch01-demo", "experiments", "1.1-demo", "main.go"), expSrc)

	mdPath := filepath.Join(dir, "ch01-demo", "ch01-demo.md")
	mdClean := "# Demo\n\n" +
		"<!-- include: experiments/1.1-demo/main.go -->\n" +
		"```go\n" + expSrc + "```\n\n" +
		"<!-- include: experiments/1.1-demo/main.go lines=5-7 -->\n" +
		"```go\nfunc main() {\n\tfmt.Println(1)\n}\n```\n"
	writeFile(mdPath, mdClean)

	// CHK-08: 1-byte drift in full include and 1-byte drift in lines=A-B include.
	mdDriftFull := strings.Replace(mdClean, "fmt.Println(1)", "fmt.Println(2)", 1)
	writeFile(mdPath, mdDriftFull)
	codeFull, outFull, err := runCmd(dir, nil, courseBin, "inline", "-check", "ch01-demo/ch01-demo.md")
	if err != nil {
		fatal("CHK-08 full", err)
	}
	mdDriftSlice := strings.Replace(mdClean, "```go\nfunc main() {\n\tfmt.Println(1)", "```go\nfunc main() {\n\tfmt.Println(9)", 1)
	writeFile(mdPath, mdDriftSlice)
	codeSlice, outSlice, err := runCmd(dir, nil, courseBin, "inline", "-check", "ch01-demo/ch01-demo.md")
	if err != nil {
		fatal("CHK-08 slice", err)
	}
	held08 := codeFull != 0 && strings.Contains(outFull, "FAIL inline -check") &&
		codeSlice != 0 && strings.Contains(outSlice, "FAIL inline -check")
	report(
		"CHK-08",
		"course inline -check with 1-byte drift in full include and in lines=5-7 slice include",
		"non-zero exit and FAIL on both full and partial include drift",
		fmt.Sprintf("full(exit=%d): %q ; slice(exit=%d): %q", codeFull, strings.TrimSpace(outFull), codeSlice, strings.TrimSpace(outSlice)),
		held08,
	)

	// CHK-09: clean passes, then 1-byte trailing space added to one line fails.
	writeFile(mdPath, mdClean)
	codeClean, outClean, err := runCmd(dir, nil, courseBin, "inline", "-check", "ch01-demo/ch01-demo.md")
	if err != nil {
		fatal("CHK-09 clean", err)
	}
	mdTrailingSpace := strings.Replace(mdClean, "package main\n", "package main \n", 1)
	writeFile(mdPath, mdTrailingSpace)
	codeSpace, outSpace, err := runCmd(dir, nil, courseBin, "inline", "-check", "ch01-demo/ch01-demo.md")
	if err != nil {
		fatal("CHK-09 trailing space", err)
	}
	held09 := codeClean == 0 && strings.Contains(outClean, "PASS inline -check") &&
		codeSpace != 0 && strings.Contains(outSpace, "FAIL inline -check")
	report(
		"CHK-09",
		"course inline -check on clean file vs 1-byte trailing space added inside code fence",
		"clean exits 0 (PASS), trailing space exits non-zero (FAIL)",
		fmt.Sprintf("clean(exit=%d): %q ; space(exit=%d): %q", codeClean, strings.TrimSpace(outClean), codeSpace, strings.TrimSpace(outSpace)),
		held09,
	)

	// CHK-10: included Go file containing literal triple backticks inside a raw string.
	backtickGo := "package main\n\nconst fence = `\n" + "```" + "\n`\n"
	writeFile(filepath.Join(dir, "ch01-demo", "experiments", "1.2-ticks", "main.go"), backtickGo)
	mdTicks := "# Ticks\n\n<!-- include: experiments/1.2-ticks/main.go -->\n"
	mdTicksPath := filepath.Join(dir, "ch01-demo", "ticks.md")
	writeFile(mdTicksPath, mdTicks)
	codeRewrite, outRewrite, err := runCmd(dir, nil, courseBin, "inline", "ch01-demo/ticks.md")
	if err != nil {
		fatal("CHK-10 rewrite", err)
	}
	codeCheckOK, outCheckOK, err := runCmd(dir, nil, courseBin, "inline", "-check", "ch01-demo/ticks.md")
	if err != nil {
		fatal("CHK-10 check ok", err)
	}
	rewritten, err := os.ReadFile(mdTicksPath)
	if err != nil {
		fatal("CHK-10 read rewritten", err)
	}
	drifted := strings.Replace(string(rewritten), "const fence", "const fencx", 1)
	writeFile(mdTicksPath, drifted)
	codeCheckDrift, outCheckDrift, err := runCmd(dir, nil, courseBin, "inline", "-check", "ch01-demo/ticks.md")
	if err != nil {
		fatal("CHK-10 check drift", err)
	}
	held10 := codeRewrite == 0 && codeCheckOK == 0 && strings.Contains(string(rewritten), "````go") &&
		codeCheckDrift != 0 && strings.Contains(outCheckDrift, "FAIL inline -check")
	report(
		"CHK-10",
		"course inline + inline -check on Go file containing triple backticks (must use 4-backtick fence and catch 1-byte drift)",
		"rewrite uses ````go and passes -check; 1-byte drift fails -check",
		fmt.Sprintf("rewrite=%d (%q) checkOK=%d (%q) checkDrift=%d (%q)",
			codeRewrite, strings.TrimSpace(outRewrite), codeCheckOK, strings.TrimSpace(outCheckOK), codeCheckDrift, strings.TrimSpace(outCheckDrift)),
		held10,
	)
}

func runSourcesAndSpansAttacks(courseBin, dir, mockTree string) {
	initTempRepo(dir)
	if err := os.RemoveAll(mockTree); err != nil {
		fatal("clean mock tree", err)
	}
	pin := sqlite.Current()
	writeFile(filepath.Join(mockTree, "manifest.uuid"), pin.FossilID+"\n")

	// Build a 60-line synthetic C source file with a known function at line 40 and a prefix collision at line 10.
	var cLines []string
	for i := 1; i <= 60; i++ {
		switch i {
		case 10:
			cLines = append(cLines, "static int unixLockHelper(int fd) {")
		case 40:
			cLines = append(cLines, "static int unixLock(int fd, int level) {")
		case 55:
			cLines = append(cLines, "  return 0;")
		case 56:
			cLines = append(cLines, "}")
		default:
			cLines = append(cLines, fmt.Sprintf("/* line %d */", i))
		}
	}
	pinnedC := strings.Join(cLines, "\n") + "\n"
	pinnedSHA := sha256Hex(pinnedC)
	writeFile(filepath.Join(mockTree, "src", "os_unix.c"), pinnedC)

	env := []string{"SQLITE_SRC=" + mockTree}
	chapDir := filepath.Join(dir, "ch01-test")
	writeFile(filepath.Join(chapDir, "ch01-test.md"), "# Test\n")
	writeFile(filepath.Join(chapDir, "os_unix.c"), pinnedC)
	writeFile(filepath.Join(chapDir, "SOURCES.sha256"), pinnedSHA+"  src/os_unix.c\n")

	// CHK-11: clean check sources passes; 1-byte modification in copied file fails.
	codeClean, outClean, err := runCmd(dir, env, courseBin, "check", "sources")
	if err != nil {
		fatal("CHK-11 clean", err)
	}
	modifiedC := strings.Replace(pinnedC, "return 0;", "return 1;", 1)
	writeFile(filepath.Join(chapDir, "os_unix.c"), modifiedC)
	codeMod, outMod, err := runCmd(dir, env, courseBin, "check", "sources")
	if err != nil {
		fatal("CHK-11 modified", err)
	}
	held11 := codeClean == 0 && strings.Contains(outClean, "PASS check sources") &&
		codeMod != 0 && strings.Contains(outMod, "FAIL check sources") && strings.Contains(outMod, "differs from the pinned")
	report(
		"CHK-11",
		"course check sources on clean copy vs 1-byte modified os_unix.c copy",
		"clean exits 0 (PASS), 1-byte modified copy exits non-zero (FAIL)",
		fmt.Sprintf("clean(exit=%d): %q ; modified(exit=%d): %q", codeClean, strings.TrimSpace(outClean), codeMod, strings.TrimSpace(outMod)),
		held11,
	)

	// Restore exact copy; test tampered sha256 and unlisted .c/.go files at chapter root.
	writeFile(filepath.Join(chapDir, "os_unix.c"), pinnedC)
	badSHA := "0" + pinnedSHA[1:]
	if pinnedSHA[0] == '0' {
		badSHA = "1" + pinnedSHA[1:]
	}
	writeFile(filepath.Join(chapDir, "SOURCES.sha256"), badSHA+"  src/os_unix.c\n")
	codeBadSHA, outBadSHA, err := runCmd(dir, env, courseBin, "check", "sources")
	if err != nil {
		fatal("CHK-12 bad sha", err)
	}
	writeFile(filepath.Join(chapDir, "SOURCES.sha256"), pinnedSHA+"  src/os_unix.c\n")
	writeFile(filepath.Join(chapDir, "stray.go"), "package stray\n")
	codeStray, outStray, err := runCmd(dir, env, courseBin, "check", "sources")
	if err != nil {
		fatal("CHK-12 stray go", err)
	}
	os.Remove(filepath.Join(chapDir, "stray.go"))
	held12 := codeBadSHA != 0 && strings.Contains(outBadSHA, "FAIL check sources") &&
		codeStray != 0 && strings.Contains(outStray, "FAIL check sources")
	report(
		"CHK-12",
		"course check sources with 1-hex-char altered SOURCES.sha256 and with stray .go file at chapter root",
		"both altered sha256 and stray chapter-root file exit non-zero (FAIL)",
		fmt.Sprintf("badSHA(exit=%d): %q ; stray(exit=%d): %q", codeBadSHA, strings.TrimSpace(outBadSHA), codeStray, strings.TrimSpace(outStray)),
		held12,
	)

	// CHK-13: check spans: clean pass vs shifted down 1 line, shifted up 12 lines, end past EOF, and prefix collision.
	spansPath := filepath.Join(chapDir, "SPANS.txt")
	writeFile(spansPath, "src/os_unix.c 40 56 unixLock\n")
	codeSpOK, outSpOK, err := runCmd(dir, env, courseBin, "check", "spans")
	if err != nil {
		fatal("CHK-13 ok", err)
	}
	badSpanCases := []struct {
		label string
		line  string
	}{
		{"shift-down-1", "src/os_unix.c 41 56 unixLock\n"},
		{"shift-up-12", "src/os_unix.c 28 56 unixLock\n"},
		{"end-past-eof", "src/os_unix.c 40 99 unixLock\n"},
		{"prefix-collision", "src/os_unix.c 10 20 unixLock\n"},
	}
	var results []string
	allBadCaught := true
	for _, tc := range badSpanCases {
		writeFile(spansPath, tc.line)
		c, o, runErr := runCmd(dir, env, courseBin, "check", "spans")
		if runErr != nil {
			fatal("CHK-13 "+tc.label, runErr)
		}
		caught := c != 0 && strings.Contains(o, "FAIL check spans")
		if !caught {
			allBadCaught = false
		}
		results = append(results, fmt.Sprintf("%s:exit=%d", tc.label, c))
	}
	held13 := codeSpOK == 0 && strings.Contains(outSpOK, "PASS check spans") && allBadCaught
	report(
		"CHK-13",
		"course check spans on valid span vs shifted down 1 line, shifted up 12 lines, end>EOF, and unixLockHelper prefix collision",
		"valid exits 0 (PASS); all four shifted/invalid/colliding spans exit non-zero (FAIL)",
		fmt.Sprintf("clean(exit=%d) ; bad=[%s]", codeSpOK, strings.Join(results, ", ")),
		held13,
	)
}
