// Command site-link-and-check runs "must be able to fail" attacks and HTML
// link-checker falsification attacks against internal/site (Build, CheckSite,
// link rewriting, anchor validation, and cmd/course site -check).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/site"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
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

func makeScratchRepo(dir string, extraFiles map[string]string) {
	if err := os.RemoveAll(dir); err != nil {
		fatal("clean "+dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir "+dir, err)
	}
	mustGit(dir, "init", "-q", "-b", "main")
	mustGit(dir, "config", "user.name", "Adv Test")
	mustGit(dir, "config", "user.email", "adv@example.invalid")

	baseFiles := map[string]string{
		"go.mod":                   "module " + sqlitex.ModulePath + "\n\ngo 1.26.0\n",
		".gitignore":               "/_work/\n/_site/\n",
		"README.md":                "# Test Book\n\nStart at [ch1](ch01-alpha/ch01-alpha.md) or [reading path](#reading-path).\n",
		"OUTLINE.md":               "# Outline\n\n## 1. Scope\n\n## 2. Rulings\n",
		"ch01-alpha/ch01-alpha.md": "# Chapter 1 — Alpha\n\n## 1. Model\n\n### Experiment 1.1 — First (`1.1-first`)\n\nText.\n\n## 8. Knowledge check\n\nSee [answers.md](answers.md).\n",
		"ch01-alpha/answers.md":    "# Chapter 1 Answers\n\n1. Answer.\n",
		"ch01-alpha/experiments/1.1-first/main.go": "package main\n",
		"ch01-alpha/evidence/1.1-first.txt":        "ok\n",
		"ch02-beta/ch02-beta.md":                   "# Chapter 2 — Beta\n\n## 1. Overview\n\nBack to [ch1](../ch01-alpha/ch01-alpha.md#1-model).\n",
		"internal/x/x.go":                          "package x\n",
		"docs/knowledge/note.md":                   "# Note\n\n## Part A\n\n## Part B\n",
	}
	for k, v := range extraFiles {
		baseFiles[k] = v
	}
	for k, v := range baseFiles {
		writeFile(filepath.Join(dir, filepath.FromSlash(k)), v)
	}
	mustGit(dir, "add", "-A")
	mustGit(dir, "commit", "-q", "-m", "init")
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-pub", "site-link-and-check")
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

	attackBrokenRelativeLinks(courseBin, workDir)
	attackMissingAnchors(courseBin, workDir)
	attackBrokenImageAndEmptyLink(courseBin, workDir)
	attackSingleQuotedHTML(courseBin, workDir)
	attackGhostIDInComment(courseBin, workDir)
	attackHrefInComment(courseBin, workDir)
	attackQueryStringLinks(courseBin, workDir)
	attackBasePrefixAndTraversal(workDir)
	attackRepoBranchAndSpaces(courseBin, workDir)
	attackDuplicateIDs(courseBin, workDir)
}

func attackBrokenRelativeLinks(courseBin, workDir string) {
	dir := filepath.Join(workDir, "broken-rel")
	makeScratchRepo(dir, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"[missing](does-not-exist.go)\n" +
			"[ignored](../_work/secret.txt)\n" +
			"[escape](../../outside.txt)\n" +
			"[abs](/etc/passwd)\n\n" +
			"<!-- include: experiments/missing/main.go -->\n```go\npackage main\n```\n",
	})
	// Create the gitignored file on disk to prove ignored files are still rejected as link targets.
	writeFile(filepath.Join(dir, "_work", "secret.txt"), "ignored\n")

	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	needles := []string{"does-not-exist.go", "_work/secret.txt", "leaves the repository", "absolute link /etc/passwd", "experiments/missing/main.go"}
	var missing []string
	for _, n := range needles {
		if !strings.Contains(out, n) {
			missing = append(missing, n)
		}
	}
	held := code == 1 && len(missing) == 0
	got := fmt.Sprintf("exit=%d; all 5 violations reported", code)
	if !held {
		got = fmt.Sprintf("exit=%d; missed=%v; out=%s", code, missing, strings.TrimSpace(out))
	}
	report("LINK-01",
		"Must-be-able-to-fail: inject missing file, gitignored file (present on disk), repo escape, absolute link, and broken include marker",
		"course site -check exits 1 and reports all 5 broken targets",
		got, held)
}

func attackMissingAnchors(courseBin, workDir string) {
	dir := filepath.Join(workDir, "missing-anchors")
	makeScratchRepo(dir, map[string]string{
		"README.md": "# Test Book\n\nBad generated anchor [bad-gen](#reading-path-99).\n",
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"[same-page](#no-such-section)\n" +
			"[cross-chap](../ch01-alpha/ch01-alpha.md#no-such-ch1)\n" +
			"[self-file](ch02-beta.md#no-such-self)\n" +
			"[outline-bad](../OUTLINE.md#no-such-ruling)\n" +
			"[note-bad](../docs/knowledge/note.md#no-such-part)\n",
	})

	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	needles := []string{"#reading-path-99", "#no-such-section", "#no-such-ch1", "#no-such-self", "#no-such-ruling", "#no-such-part"}
	var missing []string
	for _, n := range needles {
		if !strings.Contains(out, n) {
			missing = append(missing, n)
		}
	}
	held := code == 1 && len(missing) == 0
	got := fmt.Sprintf("exit=%d; all 6 missing anchors caught", code)
	if !held {
		got = fmt.Sprintf("exit=%d; missed=%v; out=%s", code, missing, strings.TrimSpace(out))
	}
	report("LINK-02",
		"Must-be-able-to-fail: inject missing anchors on README, same-page, cross-chapter, self-file, outline, and knowledge page",
		"course site -check exits 1 and flags all 6 missing anchors",
		got, held)
}

func attackBrokenImageAndEmptyLink(courseBin, workDir string) {
	dir := filepath.Join(workDir, "img-and-empty")
	makeScratchRepo(dir, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n![diagram](missing-diagram.png)\n\n[empty]()\n",
	})
	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	hasImg := strings.Contains(out, "missing-diagram.png")
	hasEmpty := strings.Contains(out, "empty link")
	held := code == 1 && hasImg && hasEmpty
	report("LINK-03",
		"Must-be-able-to-fail: inject broken markdown image ![diagram](missing-diagram.png) and empty link [empty]()",
		"course site -check exits 1 and catches both missing image and empty link",
		fmt.Sprintf("exit=%d, caughtImage=%v, caughtEmpty=%v", code, hasImg, hasEmpty),
		held)
}

func attackSingleQuotedHTML(courseBin, workDir string) {
	// Goldmark runs with html.WithUnsafe(), so raw HTML passes verbatim into
	// rendered HTML. Single-quoted href/src is valid HTML5.
	dir := filepath.Join(workDir, "single-quote-html")
	makeScratchRepo(dir, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"<a href='/book/does-not-exist/'>broken single-quoted link</a>\n" +
			"<img src='/book/missing.png' alt='broken'>\n",
	})
	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	held := code == 1 && strings.Contains(out, "does-not-exist") && strings.Contains(out, "missing.png")
	report("LINK-04",
		"Place valid HTML5 single-quoted broken links (<a href='/book/does-not-exist/'> and <img src='/book/missing.png'>) in markdown",
		"site -check fails on any broken internal link in rendered HTML",
		fmt.Sprintf("exit=%d; out=%s", code, strings.TrimSpace(out)),
		held)
}

func attackGhostIDInComment(courseBin, workDir string) {
	// An HTML comment <!-- id="ghost-anchor" --> does NOT create a DOM element
	// with id="ghost-anchor"; clicking #ghost-anchor in a browser goes nowhere.
	dir := filepath.Join(workDir, "ghost-id")
	makeScratchRepo(dir, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"<!-- id=\"ghost-anchor\" -->\n\n" +
			"Jump to [ghost](#ghost-anchor).\n",
	})
	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	held := code == 1 && strings.Contains(out, "ghost-anchor")
	report("LINK-05",
		"Link to [ghost](#ghost-anchor) where id=\"ghost-anchor\" only exists inside an HTML comment <!-- id=\"ghost-anchor\" -->",
		"site -check rejects #ghost-anchor because HTML comments do not define DOM anchors",
		fmt.Sprintf("exit=%d; out=%s", code, strings.TrimSpace(out)),
		held)
}

func attackHrefInComment(courseBin, workDir string) {
	// Build-notes §3: "Other HTML comments pass through as HTML comments, invisible."
	dir := filepath.Join(workDir, "href-in-comment")
	makeScratchRepo(dir, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"<!-- draft note: <a href=\"/book/not-yet-built/\">todo</a> -->\n\n" +
			"All real links here are valid.\n",
	})
	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	held := code == 0
	report("LINK-06",
		"Include an HTML comment containing href=\"/book/not-yet-built/\" on an otherwise valid page",
		"invisible HTML comments do not cause false-positive broken link failures in site -check",
		fmt.Sprintf("exit=%d; out=%s", code, strings.TrimSpace(out)),
		held)
}

func attackQueryStringLinks(courseBin, workDir string) {
	// repocheck.ResolveRelativeLink strips ?query before checking existence;
	// check whether internal/site handles ?query on internal or repo links consistently.
	dir := filepath.Join(workDir, "query-links")
	makeScratchRepo(dir, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"See [ch1 with query](../ch01-alpha/ch01-alpha.md?v=1#1-model) and [code raw](../internal/x/x.go?plain=1).\n",
	})
	code, out, _ := runCmd(dir, courseBin, "site", "-check", "-base", "/book/")
	held := code == 0
	report("LINK-07",
		"Use relative links with query strings (../ch01-alpha/ch01-alpha.md?v=1#1-model and ../internal/x/x.go?plain=1)",
		"query strings on valid relative links are preserved or handled without falsely failing resolution (consistent with repocheck.ResolveRelativeLink)",
		fmt.Sprintf("exit=%d; out=%s", code, strings.TrimSpace(out)),
		held)
}

func attackBasePrefixAndTraversal(workDir string) {
	dir := filepath.Join(workDir, "base-boundary")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir base-boundary", err)
	}
	// Crafted rendered HTML testing subtle base prefix and traversal checks in site.CheckSite.
	writeFile(filepath.Join(dir, "index.html"),
		`<a href="/book">exact-no-slash</a> `+
			`<a href="/book/">exact-slash</a> `+
			`<a href="/book2/">prefix-collision</a> `+
			`<a href="../../../etc/passwd">traversal</a> `+
			`<a href="/book/%ZZ">bad-escape</a>`)

	res, err := site.CheckSite(dir, "/book/")
	if err != nil {
		fatal("CheckSite", err)
	}
	var msgs []string
	for _, p := range res.Problems {
		msgs = append(msgs, p.String())
	}
	joined := strings.Join(msgs, " ; ")

	// /book and /book/ must be accepted; /book2/, traversal, and %ZZ must all be rejected.
	hasBook2 := strings.Contains(joined, "/book2/")
	hasTrav := strings.Contains(joined, "leaves the site")
	hasBadEsc := strings.Contains(joined, "malformed link")
	falsePos := strings.Contains(joined, `"/book"`) || len(res.Problems) != 3

	held := hasBook2 && hasTrav && hasBadEsc && !falsePos
	report("LINK-08",
		"Test CheckSite base='/book/' with /book, /book/, /book2/ (prefix confusion), ../../../etc/passwd (escape), and /book/%ZZ",
		"/book and /book/ pass; /book2/, traversal, and malformed percent-escape are all rejected (3 problems)",
		fmt.Sprintf("problems=%d (%s)", len(res.Problems), joined),
		held)
}

func attackRepoBranchAndSpaces(courseBin, workDir string) {
	dir := filepath.Join(workDir, "repo-branch-spaces")
	makeScratchRepo(dir, map[string]string{
		"docs/my file.txt": "hello\n",
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n## 1. Overview\n\n" +
			"[spaced](../docs/my%20file.txt) and [spaced-angle](<../docs/my file.txt>) and [dir](../internal/x/) and [root](..).\n",
	})
	outDir := filepath.Join(workDir, "repo-branch-out")
	code, out, _ := runCmd(dir, courseBin, "site",
		"-out", outDir,
		"-base", "/custom/",
		"-repo", "https://github.com/fork/book/",
		"-branch", "release/v1.0")
	if code != 0 {
		report("LINK-09",
			"Build with custom -repo (trailing slash), -branch (containing '/'), and links to file with space, dir, and repo root",
			"build succeeds with clean blob/tree URLs, no double slashes, percent-encoded spaces",
			fmt.Sprintf("exit=%d out=%s", code, strings.TrimSpace(out)),
			false)
		return
	}
	data, err := os.ReadFile(filepath.Join(outDir, "ch02-beta", "index.html"))
	if err != nil {
		fatal("read ch02-beta/index.html", err)
	}
	htmlStr := string(data)
	wantFile := `href="https://github.com/fork/book/blob/release/v1.0/docs/my%20file.txt"`
	wantDir := `href="https://github.com/fork/book/tree/release/v1.0/internal/x"`
	wantRoot := `href="https://github.com/fork/book/tree/release/v1.0/"`
	hasDoubleSlash := strings.Contains(htmlStr, "book//blob") || strings.Contains(htmlStr, "book//tree")
	ok := strings.Count(htmlStr, wantFile) == 2 &&
		strings.Contains(htmlStr, wantDir) &&
		strings.Contains(htmlStr, wantRoot) &&
		!hasDoubleSlash

	report("LINK-09",
		"Build with -repo https://github.com/fork/book/ (trailing slash), -branch release/v1.0, spaced filenames, dir, and repo root (..)",
		"all links emit clean blob/tree URLs without //, spaces encoded as %20, dir and root use /tree/",
		fmt.Sprintf("spacedCount=%d, hasDir=%v, hasRoot=%v, hasDoubleSlash=%v",
			strings.Count(htmlStr, wantFile), strings.Contains(htmlStr, wantDir), strings.Contains(htmlStr, wantRoot), hasDoubleSlash),
		ok)
}

func attackDuplicateIDs(courseBin, workDir string) {
	// Part A: README with literal "## Reading path" and "## Reference" headings
	// must disambiguate the generated section headings to reading-path-1 / reference-1.
	dir := filepath.Join(workDir, "dup-ids")
	makeScratchRepo(dir, map[string]string{
		"README.md": "# Test Book\n\n## Reading path\n\nIntro.\n\n## Reference\n\nRefs.\n",
	})
	outDir := filepath.Join(workDir, "dup-ids-out")
	code, out, _ := runCmd(dir, courseBin, "site", "-out", outDir, "-base", "/book/")
	homeBytes, _ := os.ReadFile(filepath.Join(outDir, "index.html"))
	home := string(homeBytes)
	disambigOK := code == 0 &&
		strings.Contains(home, `<h2 id="reading-path">Reading path</h2>`) &&
		strings.Contains(home, `<h2 id="reading-path-1">Reading path</h2>`) &&
		strings.Contains(home, `<h2 id="reference">Reference</h2>`) &&
		strings.Contains(home, `<h2 id="reference-1">Reference</h2>`)
	report("LINK-10a",
		"Put literal '## Reading path' and '## Reference' headings inside README.md",
		"generated sections share README's slugger and become reading-path-1 and reference-1 with no ID collision",
		fmt.Sprintf("exit=%d, disambiguated=%v (%s)", code, disambigOK, strings.TrimSpace(out)),
		disambigOK)

	// Part B: Raw HTML <div id="1-model"> colliding with markdown ## 1. Model.
	// Check whether Build or CheckSite detects duplicate DOM id attributes.
	dirB := filepath.Join(workDir, "dup-ids-raw")
	makeScratchRepo(dirB, map[string]string{
		"ch02-beta/ch02-beta.md": "# Chapter 2 — Beta\n\n<div id=\"1-overview\">raw</div>\n\n## 1. Overview\n\nLink [go](#1-overview).\n",
	})
	codeB, outB, _ := runCmd(dirB, courseBin, "site", "-check", "-base", "/book/")
	// CheckSite currently stores ids in map[string]bool without checking duplicates.
	heldB := codeB != 0
	report("LINK-10b",
		"Collide raw HTML <div id=\"1-overview\"> with markdown heading ## 1. Overview on the same page",
		"site -check detects duplicate DOM id=\"1-overview\" on a page",
		fmt.Sprintf("exit=%d; out=%s", codeB, strings.TrimSpace(outB)),
		heldB)
}
