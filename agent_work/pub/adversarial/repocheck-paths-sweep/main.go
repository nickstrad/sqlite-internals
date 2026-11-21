// Command repocheck-paths-sweep runs "must be able to fail" attacks and
// falsification attacks against internal/repocheck/paths.go and
// cmd/course check paths (addendum C: machine-local paths, unresolved relative
// markdown links, binary/ignored handling, and external link reporting).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/repocheck"
	"github.com/nickstrad/walfleet/internal/sqlitex"
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

func runCmd(dir string, name string, args ...string) (int, string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
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

func mustGit(dir string, args ...string) {
	code, out, err := runCmd(dir, "git", args...)
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("git %s (exit %d): %s", strings.Join(args, " "), code, strings.TrimSpace(out)), err)
	}
}

func writeFile(p, content string) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fatal("mkdir for "+p, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fatal("write "+p, err)
	}
}

func initGitRepo(dir string, files map[string]string) {
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean "+dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir "+dir, err)
	}
	mustGit(dir, "init", "-q", "-b", "main")
	mustGit(dir, "config", "user.name", "Adv Test")
	mustGit(dir, "config", "user.email", "adv@example.invalid")
	writeFile(filepath.Join(dir, "go.mod"), "module "+sqlitex.ModulePath+"\n\ngo 1.26.0\n")
	for rel, content := range files {
		writeFile(filepath.Join(dir, filepath.FromSlash(rel)), content)
	}
	mustGit(dir, "add", "-A")
	mustGit(dir, "commit", "-q", "-m", "init")
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-pub", "repocheck-paths-sweep")
	if err := os.RemoveAll(workDir); err != nil {
		fatal("clean workDir", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir workDir", err)
	}

	courseBin := filepath.Join(workDir, "course")
	code, out, err := runCmd(repoRoot, "go", "build", "-buildvcs=false", "-o", courseBin, "./cmd/course")
	if err != nil || code != 0 {
		fatal(fmt.Sprintf("build cmd/course (exit %d): %s", code, out), err)
	}

	attackLiveRepoSweep(repoRoot, courseBin)
	attackMachinePathsMustFail(courseBin, workDir)
	attackMachinePathsBareAndCase(workDir)
	attackRelativeLinksMustFail(courseBin, workDir)
	attackInlineCodeBacktickParity(workDir)
	attackMarkdownLinkSyntaxEdges(workDir)
	attackFenceStateEdges(workDir)
	attackPublicReferenceAllowlistAndTraversal(courseBin, workDir)
	attackBinaryIgnoredAndDeletedTracked(courseBin, workDir)
}

func attackLiveRepoSweep(repoRoot, courseBin string) {
	// Run course check paths against the real repository checkout.
	code, out, err := runCmd(repoRoot, courseBin, "check", "paths")
	if err != nil {
		fatal("run course check paths", err)
	}
	var failLines []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "FAIL ") || strings.HasPrefix(line, "  ") {
			failLines = append(failLines, t)
		}
	}
	held := code == 0
	got := "exit=0 PASS"
	if !held {
		got = fmt.Sprintf("exit=%d; %s", code, strings.Join(failLines, " ; "))
	}
	report("PATH-01",
		"Run `course check paths` on the actual repository checkout (which includes verbatim SOURCES.sha256 SQLite C copies from ch04/ch05/ch07)",
		"check paths passes (exit 0) on the clean repository without false-failing on unmodified pinned upstream SQLite C comments",
		got, held)
}

func attackMachinePathsMustFail(courseBin, workDir string) {
	dir := filepath.Join(workDir, "machine-must-fail")
	rootFrag := "/" + "root" + "/"
	homeFrag := "/" + "home" + "/alice/"
	macFrag := "/" + "Users" + "/bob/"
	initGitRepo(dir, map[string]string{
		".gitignore":              "/_work/\n",
		"README.md":               "# Book\n\nPath in prose: `" + rootFrag + "src`\n\n```sh\nSQLITE_SRC=" + homeFrag + "sqlite\n```\n",
		"internal/sample.go":      "package sample\n\nconst p = \"file://" + rootFrag + "x\"\n",
		"internal/sample_test.go": "package sample\n\nconst q = \"PATH=/usr/bin:" + macFrag + "bin\"\n",
		"compose.yaml":            "volumes:\n  - " + homeFrag + "data:/data\n",
		// Allowed false-positive guards:
		"docs/safe.md": "OK: https://example.org" + rootFrag + "x and a" + rootFrag + "b and /home/<name>/\n",
		// Excluded paths:
		"agent_work/work.md": rootFrag + "allowed-here\n",
		"kb/vendored.md":     macFrag + "allowed-here\n",
		"STATE.md":           homeFrag + "allowed-here\n",
	})
	// Untracked and ignored files on disk containing machine paths must never be scanned.
	writeFile(filepath.Join(dir, "untracked.md"), rootFrag+"untracked\n")
	writeFile(filepath.Join(dir, "_work/ignored.md"), homeFrag+"ignored\n")

	code, out, _ := runCmd(dir, courseBin, "check", "paths")
	// Extract only FAIL problem lines (ignoring EXTERNAL/INFO lines, since
	// https://example.org/root/x in docs/safe.md is legitimately reported as EXTERNAL).
	var failLines []string
	inFail := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FAIL ") {
			inFail = true
			failLines = append(failLines, line)
			continue
		}
		if inFail && strings.HasPrefix(line, "  ") {
			failLines = append(failLines, line)
		}
	}
	failBlock := strings.Join(failLines, "\n")
	wantLocations := []string{
		"README.md:3",
		"README.md:6",
		"internal/sample.go:3",
		"internal/sample_test.go:3",
		"compose.yaml:2",
	}
	forbidden := []string{"docs/safe.md", "agent_work/", "kb/", "STATE.md", "untracked.md", "_work/"}
	var issues []string
	if code != 1 {
		issues = append(issues, fmt.Sprintf("exit=%d want 1", code))
	}
	for _, w := range wantLocations {
		if !strings.Contains(failBlock, w) {
			issues = append(issues, "missed "+w)
		}
	}
	for _, f := range forbidden {
		if strings.Contains(failBlock, f) {
			issues = append(issues, "false-flagged "+f)
		}
	}
	held := len(issues) == 0
	got := "exit=1; caught all 5 machine paths across md/code-fence/go/test.go/yaml and ignored safe/excluded/untracked files"
	if !held {
		got = strings.Join(issues, "; ")
	}
	report("PATH-02",
		"Must-be-able-to-fail: inject /root/, /home/alice/, /Users/bob/ across .md (prose+fence), .go, _test.go, .yaml plus safe URLs, excluded paths, and untracked/ignored files",
		"course check paths exits 1, flags all 5 reader-facing hits, and ignores safe URLs, agent_work/, kb/, STATE.md, untracked, and ignored files",
		got, held)
}

func attackMachinePathsBareAndCase(workDir string) {
	dir := filepath.Join(workDir, "machine-bare")
	line := "Run `cd /" + "root` or set `HOME=/" + "home/alice` or `DIR=/" + "Users/bob` or macOS lowercase `/" + "users/bob/src/`."
	initGitRepo(dir, map[string]string{
		"docs/bare.md": line + "\n",
	})
	sweep, err := repocheck.SweepPaths(dir)
	if err != nil {
		fatal("SweepPaths bare", err)
	}
	wantPaths := []string{"/" + "root", "/" + "home/alice", "/" + "Users/bob", "/" + "users/bob/"}
	gotPaths := repocheck.MachinePaths(line)
	matchedPrefix := ""
	if len(sweep.Problems) == 1 {
		matchedPrefix, _, _ = strings.Cut(sweep.Problems[0].Message, ": ")
	}
	allInSweep := len(sweep.Problems) == 1 && matchedPrefix == "mentions a machine-local path "+strings.Join(wantPaths, ", ")
	held := allInSweep && strings.Join(gotPaths, ",") == strings.Join(wantPaths, ",")
	report("PATH-03",
		"Place home-directory paths without trailing slash ('cd /root', 'HOME=/home/alice', 'DIR=/Users/bob') and lowercase macOS '/users/bob/src/' in a tracked .md file",
		"all 4 machine-local home directory references are detected (reported together on that line)",
		fmt.Sprintf("problems=%d, matched=%v (%v)", len(sweep.Problems), gotPaths, sweep.Problems),
		held)
}

func attackRelativeLinksMustFail(courseBin, workDir string) {
	dir := filepath.Join(workDir, "rel-must-fail")
	initGitRepo(dir, map[string]string{
		".gitignore": "/_work/\n",
		"README.md": "# Book\n\n" +
			"Valid: [doc](docs/ok.md#sec) [dir](docs/) [ext](https://sqlite.org/wal.html) [mail](mailto:a@b.invalid) [anchor](#book)\n" +
			"Inline code ignored: `[ign](nope-code.md)`\n" +
			"```md\n[ign-fence](nope-fence.md)\n```\n" +
			"~~~md\n[ign-tilde](nope-tilde.md)\n~~~\n" +
			"> ```md\n> [ign-quote](nope-quote.md)\n> ```\n\n" +
			"Broken: [missing](missing.md) [out](../../outside.md) [abs](/etc/hosts) [untracked](loose.md) [ignored](_work/gen.txt) ![img](missing.png)\n\n" +
			"[ref-ok]: ./docs/ok.md\n" +
			"[ref-bad]: nowhere.md\n",
		"docs/ok.md": "# Ok\n",
	})
	writeFile(filepath.Join(dir, "loose.md"), "# Loose untracked\n")
	writeFile(filepath.Join(dir, "_work/gen.txt"), "ignored\n")

	code, out, _ := runCmd(dir, courseBin, "check", "paths")
	wantBroken := []string{"missing.md", "../../outside.md", "/etc/hosts", "loose.md", "_work/gen.txt", "missing.png", "nowhere.md"}
	mustNotFlag := []string{"docs/ok.md", "nope-code.md", "nope-fence.md", "nope-tilde.md", "nope-quote.md"}
	var issues []string
	if code != 1 {
		issues = append(issues, fmt.Sprintf("exit=%d want 1", code))
	}
	for _, w := range wantBroken {
		if !strings.Contains(out, w) {
			issues = append(issues, "missed "+w)
		}
	}
	for _, f := range mustNotFlag {
		if strings.Contains(out, f) {
			issues = append(issues, "false-flagged "+f)
		}
	}
	held := len(issues) == 0
	got := "exit=1; caught all 7 broken relative links (missing, escape, abs, untracked, ignored, image, ref) and ignored fenced/inline-code links"
	if !held {
		got = strings.Join(issues, "; ")
	}
	report("PATH-04",
		"Must-be-able-to-fail: inject 7 broken relative link kinds plus valid links, inline code, and ``` / ~~~ / quoted fences",
		"course check paths exits 1, catches all 7 broken links, and produces 0 false positives",
		got, held)
}

func attackInlineCodeBacktickParity(workDir string) {
	// Bug probe: inlineCodePattern is (`+)[^`]*?(`+) without backreferences.
	// Case A (false negative): a double-backtick span holding a literal backtick (`` ` ``)
	// followed by a real broken link and another code span causes regex desync and swallows the real link.
	dirA := filepath.Join(workDir, "backtick-fn")
	initGitRepo(dirA, map[string]string{
		"README.md": "A literal backtick `` ` `` followed by [broken](missing-swallowed.md) and `ok`.\n",
	})
	sweepA, err := repocheck.SweepPaths(dirA)
	if err != nil {
		fatal("SweepPaths backtick-fn", err)
	}
	caughtSwallowed := len(sweepA.Problems) == 1 && strings.Contains(sweepA.Problems[0].Message, "missing-swallowed.md")
	report("PATH-05a",
		"Line with CommonMark double-backtick span holding a literal backtick (`` ` ``) followed by real broken link [broken](missing-swallowed.md) and `ok`",
		"SweepPaths catches [broken](missing-swallowed.md) outside the inline code spans",
		fmt.Sprintf("problems=%d (%v)", len(sweepA.Problems), sweepA.Problems),
		caughtSwallowed)

	// Case B (false positive): a double-backtick inline code span wrapping a single-backtick example link.
	dirB := filepath.Join(workDir, "backtick-fp")
	initGitRepo(dirB, map[string]string{
		"README.md": "Example syntax: ``use `[not-a-link](fake-inside-code.md)` here``.\n",
	})
	sweepB, err := repocheck.SweepPaths(dirB)
	if err != nil {
		fatal("SweepPaths backtick-fp", err)
	}
	report("PATH-05b",
		"Line with CommonMark double-backtick code span wrapping single backticks and link syntax: ``use `[not-a-link](fake-inside-code.md)` here``",
		"SweepPaths ignores text inside the double-backtick inline code span (0 problems)",
		fmt.Sprintf("problems=%d (%v)", len(sweepB.Problems), sweepB.Problems),
		len(sweepB.Problems) == 0)
}

func attackMarkdownLinkSyntaxEdges(workDir string) {
	// CommonMark valid link forms: single-quoted title, parenthesized title, multiline link,
	// reference definition with angle-bracketed path containing a space, and angle-bracketed link without spaces.
	dir := filepath.Join(workDir, "md-link-syntax")
	initGitRepo(dir, map[string]string{
		"docs/has space.md": "# Spaced\n",
		"README.md": "# Book\n\n" +
			"[single-title](missing-single.md 'title')\n" +
			"[paren-title](missing-paren.md (title))\n" +
			"[multiline](\nmissing-multi.md\n)\n" +
			"[angle-dup](<missing-dup.md>)\n\n" +
			"[ref-space-ok]: <docs/has space.md>\n" +
			"[ref-space-bad]: <missing spaced.md>\n",
	})
	sweep, err := repocheck.SweepPaths(dir)
	if err != nil {
		fatal("SweepPaths md-link-syntax", err)
	}
	var msgs []string
	for _, p := range sweep.Problems {
		msgs = append(msgs, p.String())
	}
	joined := strings.Join(msgs, " ; ")

	hasSingle := strings.Contains(joined, "missing-single.md")
	hasParen := strings.Contains(joined, "missing-paren.md")
	hasMulti := strings.Contains(joined, "missing-multi.md")
	dupProblems := 0
	for _, p := range sweep.Problems {
		if strings.Contains(p.Message, "missing-dup.md") {
			dupProblems++
		}
	}
	falsePosSpaceOK := strings.Contains(joined, "docs/has")
	hasSpaceBad := strings.Contains(joined, "missing spaced.md")

	held := hasSingle && hasParen && hasMulti && dupProblems == 1 && !falsePosSpaceOK && hasSpaceBad
	report("PATH-06",
		"Test CommonMark links: single-quoted title, paren title, multiline destination, <no-space.md> dedup, and [ref]: <path with space.md>",
		"all 4 broken links caught once each (dupProblems=1), valid [ref]: <docs/has space.md> passes",
		fmt.Sprintf("single=%v, paren=%v, multi=%v, dupProblems=%d, falsePosValidSpacedRef=%v, badSpacedRef=%v",
			hasSingle, hasParen, hasMulti, dupProblems, falsePosSpaceOK, hasSpaceBad),
		held)
}

func attackFenceStateEdges(workDir string) {
	// Case A: CommonMark §4.5: backtick fence info string cannot contain backticks, so
	// ```inline``` at column 0 is an inline code span, NOT an opening fence.
	dirA := filepath.Join(workDir, "fence-inline-triple")
	initGitRepo(dirA, map[string]string{
		"README.md": "```inline-code``` is a paragraph, not a fence.\n\n[broken-after](missing-after-triple.md)\n",
	})
	sweepA, err := repocheck.SweepPaths(dirA)
	if err != nil {
		fatal("SweepPaths fence-inline-triple", err)
	}
	caughtA := len(sweepA.Problems) == 1 && strings.Contains(sweepA.Problems[0].Message, "missing-after-triple.md")
	report("PATH-07a",
		"Line starting with inline triple-backtick code span (```inline-code```) followed by [broken-after](missing-after-triple.md)",
		"fenceState does not treat ```inline-code``` as an unclosed fence and catches missing-after-triple.md",
		fmt.Sprintf("problems=%d (%v)", len(sweepA.Problems), sweepA.Problems),
		caughtA)

	// Case B: 5-space indented ``` is an indented code block in CommonMark (fences allow at most 3 leading spaces).
	dirB := filepath.Join(workDir, "fence-indented-5")
	initGitRepo(dirB, map[string]string{
		"README.md": "Intro:\n\n     ```\n\nBack to prose: [broken-after-indent](missing-after-indent.md)\n",
	})
	sweepB, err := repocheck.SweepPaths(dirB)
	if err != nil {
		fatal("SweepPaths fence-indented-5", err)
	}
	caughtB := len(sweepB.Problems) == 1 && strings.Contains(sweepB.Problems[0].Message, "missing-after-indent.md")
	report("PATH-07b",
		"5-space indented '     ```' (CommonMark indented code block, not a fence) followed by [broken-after-indent](missing-after-indent.md)",
		"fenceState enforces CommonMark's 0-3 space indent limit and catches missing-after-indent.md",
		fmt.Sprintf("problems=%d (%v)", len(sweepB.Problems), sweepB.Problems),
		caughtB)
}

func attackPublicReferenceAllowlistAndTraversal(courseBin, workDir string) {
	// Part A: Path traversal bypass on path-prefixed entries in IsPublicReference.
	traversals := []string{
		"https://github.com/nickstrad/walfleet/../../evil/malware",
		"https://github.com/sqlite/sqlite/../evil/repo",
		"https://nickstrad.github.io/walfleet/../other-site/",
		"https://gitlab.com/cznic/../evil/project",
	}
	var bypassed []string
	for _, u := range traversals {
		if repocheck.IsPublicReference(u) {
			bypassed = append(bypassed, u)
		}
	}
	report("PATH-08a",
		"Pass dot-dot path-traversal URLs under allowlisted prefixes (e.g. github.com/nickstrad/walfleet/../../evil/malware) to IsPublicReference",
		"IsPublicReference normalizes paths and returns false for all 4 traversal URLs",
		fmt.Sprintf("bypassed=%d/4 (%v)", len(bypassed), bypassed),
		len(bypassed) == 0)

	// Part B: End-to-end external link reporting (non-failing, deduplicated, sorted, _test.go excluded).
	dir := filepath.Join(workDir, "external-report")
	initGitRepo(dir, map[string]string{
		"README.md": "# Book\n\n" +
			"Public: https://www.sqlite.org/wal.html https://github.com/nickstrad/walfleet/blob/main/README.md " +
			"https://nickstrad.github.io/walfleet/ https://pkg.go.dev/modernc.org/sqlite https://proxy.golang.org/ https://gitlab.com/cznic/sqlite\n" +
			"External: https://zeta.example/a https://alpha.example/b https://alpha.example/b\n",
		"internal/foo_test.go": "package internal\n\nvar testURL = \"https://fixture-only.example/x\"\n",
	})
	code, out, _ := runCmd(dir, courseBin, "check", "paths")
	idxAlpha := strings.Index(out, "EXTERNAL https://alpha.example/b (2 occurrence(s), first at README.md:4)")
	idxZeta := strings.Index(out, "EXTERNAL https://zeta.example/a (1 occurrence(s), first at README.md:4)")
	hasTestURL := strings.Contains(out, "fixture-only.example")
	hasPublicAsExt := strings.Contains(out, "EXTERNAL https://www.sqlite.org") || strings.Contains(out, "EXTERNAL https://pkg.go.dev")
	heldB := code == 0 && idxAlpha >= 0 && idxZeta > idxAlpha && !hasTestURL && !hasPublicAsExt
	report("PATH-08b",
		"Run `course check paths` on repo with public refs, duplicate external URLs, and a _test.go external URL",
		"exits 0; prints sorted deduplicated EXTERNAL lines with counts; hides public refs and _test.go URLs",
		fmt.Sprintf("exit=%d, sortedDedupOK=%v, testURLHidden=%v, publicHidden=%v", code, idxAlpha >= 0 && idxZeta > idxAlpha, !hasTestURL, !hasPublicAsExt),
		heldB)
}

func attackBinaryIgnoredAndDeletedTracked(courseBin, workDir string) {
	// Part A: binary skip (NUL in first 8000 bytes), tracked-but-ignored INFO, and NUL past byte 8000.
	dir := filepath.Join(workDir, "binary-and-ignored")
	initGitRepo(dir, map[string]string{
		".gitignore":    "/_tools/\n",
		"README.md":     "# Book\n",
		"docs/late.txt": strings.Repeat("a", 8050) + "\x00\n/" + "root/late-secret\n",
	})
	writeFile(filepath.Join(dir, "_tools/bin/fake"), "ELF\x00\x01/"+"root/in-binary\n")
	mustGit(dir, "add", "-f", "_tools/bin/fake")
	mustGit(dir, "commit", "-q", "-m", "add ignored binary")

	code, out, _ := runCmd(dir, courseBin, "check", "paths")
	skippedBin := strings.Contains(out, "INFO check paths: binary file skipped: _tools/bin/fake")
	flaggedIgnored := strings.Contains(out, "INFO check paths: tracked although .gitignore matches it: _tools/bin/fake")
	caughtLate := strings.Contains(out, "docs/late.txt:2")
	heldA := code == 1 && skippedBin && flaggedIgnored && caughtLate
	report("PATH-09a",
		"Test tracked-but-ignored ELF binary containing /root/ (NUL at byte 3) plus >8KB file with NUL at byte 8050 and /root/ at line 2",
		"binary skipped with both INFO lines; >8KB file past git's 8000-byte binary window is scanned and flags /root/",
		fmt.Sprintf("exit=%d, skippedBin=%v, flaggedIgnored=%v, caughtLate=%v", code, skippedBin, flaggedIgnored, caughtLate),
		heldA)

	// Part B: Tracked file deleted on disk (not yet git rm'd) still linked from README.md.
	dirB := filepath.Join(workDir, "deleted-tracked")
	initGitRepo(dirB, map[string]string{
		"README.md": "# Book\n\nSee [gone](gone.md).\n",
		"gone.md":   "# Gone\n",
	})
	if err := os.Remove(filepath.Join(dirB, "gone.md")); err != nil {
		fatal("remove gone.md", err)
	}
	codeB, outB, _ := runCmd(dirB, courseBin, "check", "paths")
	report("PATH-09b",
		"Delete tracked file gone.md from working tree without git rm while README.md links to [gone](gone.md)",
		"course check paths detects that gone.md is missing on disk and fails the broken link",
		fmt.Sprintf("exit=%d; out=%s", codeB, strings.TrimSpace(outB)),
		codeB == 1)
}
