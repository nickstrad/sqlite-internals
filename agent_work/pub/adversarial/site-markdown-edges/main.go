// Command site-markdown-edges attacks markdown rendering, YAML front matter
// stripping, GitHub heading slugging, include-marker conversion, experiment
// and rung discovery, GFM/HTML escaping, and chapter/TOC/answers edge cases
// in internal/site and cmd/course site.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

func buildFixture(workDir, name string, files map[string]string) (string, site.Result, error) {
	srcDir := filepath.Join(workDir, name+"-src")
	outDir := filepath.Join(workDir, name+"-out")
	if err := os.RemoveAll(srcDir); err != nil {
		fatal("clean srcDir", err)
	}
	if err := os.RemoveAll(outDir); err != nil {
		fatal("clean outDir", err)
	}
	var names []string
	for rel, content := range files {
		writeFile(filepath.Join(srcDir, filepath.FromSlash(rel)), content)
		names = append(names, rel)
	}
	sort.Strings(names)
	res, err := site.Build(site.Options{
		Root:    srcDir,
		Files:   names,
		Base:    "/book/",
		RepoURL: "https://github.com/example/book",
		Branch:  "main",
	}, outDir)
	return outDir, res, err
}

func readOut(outDir, rel string) string {
	data, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(rel)))
	if err != nil {
		fatal("read "+rel, err)
	}
	return string(data)
}

func main() {
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-pub", "site-markdown-edges")
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

	attackFrontMatter(workDir)
	attackHeadingSlugs(workDir)
	attackIncludeMarkers(workDir)
	attackExperimentDiscovery(workDir)
	attackRungDetection(workDir)
	attackGFMAndHTMLEscaping(workDir)
	attackChapterTOCAnswersAndGitView(courseBin, workDir)
}

func attackFrontMatter(workDir string) {
	// MD-01a: inner quote in double-quoted YAML title: title: "SQLite's 'WAL'"
	outA, _, err := buildFixture(workDir, "fm-quotes", map[string]string{
		"README.md":              "# Book\n",
		"docs/knowledge/wal.md":  "---\ntitle: \"SQLite's 'WAL'\"\ntags: [wal]\n---\n\nBody text.\n",
		"docs/knowledge/stem.md": "Plain text with no front matter and no H1 heading.\n",
	})
	if err != nil {
		fatal("fm-quotes build", err)
	}
	walHTML := readOut(outA, "knowledge/wal/index.html")
	stemHTML := readOut(outA, "knowledge/stem/index.html")
	// In HTML, ' is escaped to &#39; by html/template.
	wantTitle := "<title>SQLite&#39;s &#39;WAL&#39; · Book</title>"
	stemOK := strings.Contains(stemHTML, "<title>stem · Book</title>")
	gotTitle := extractBetween(walHTML, "<title>", "</title>")
	report("MD-01a",
		"YAML front matter title: \"SQLite's 'WAL'\" (inner single quote before closing double quote) and bare note fallback to filename stem",
		"title preserves inner quote ('SQLite's 'WAL'') and bare note falls back to 'stem'",
		fmt.Sprintf("wal <title>=%q, stemFallbackOK=%v", gotTitle, stemOK),
		strings.Contains(walHTML, wantTitle) && stemOK)

	// MD-01b: empty YAML front matter followed by content and a horizontal rule '---'.
	outB, _, err := buildFixture(workDir, "fm-empty", map[string]string{
		"README.md":               "# Book\n",
		"docs/knowledge/empty.md": "---\n---\n# Real Heading\n\nImportant paragraph.\n\n---\n\nAfter rule.\n",
	})
	if err != nil {
		fatal("fm-empty build", err)
	}
	emptyHTML := readOut(outB, "knowledge/empty/index.html")
	hasHeading := strings.Contains(emptyHTML, "Real Heading")
	hasPara := strings.Contains(emptyHTML, "Important paragraph.")
	report("MD-01b",
		"Empty YAML front matter ('---\\n---\\n') followed by '# Real Heading', a paragraph, and a markdown horizontal rule '---'",
		"empty front matter is stripped cleanly without swallowing body text before the horizontal rule",
		fmt.Sprintf("hasHeading=%v, hasPara=%v, hasAfterRule=%v", hasHeading, hasPara, strings.Contains(emptyHTML, "After rule.")),
		hasHeading && hasPara)

	// MD-01c: YAML front matter ending at EOF with no trailing newline after closing '---'.
	outC, _, err := buildFixture(workDir, "fm-eof", map[string]string{
		"README.md":             "# Book\n",
		"docs/knowledge/eof.md": "---\ntitle: Stub Note\ntags: [stub]\n---",
	})
	if err != nil {
		fatal("fm-eof build", err)
	}
	eofHTML := readOut(outC, "knowledge/eof/index.html")
	leaked := strings.Contains(eofHTML, "tags: [stub]")
	gotEOFTitle := extractBetween(eofHTML, "<title>", "</title>")
	report("MD-01c",
		"YAML front matter closed by '---' at EOF with no trailing newline",
		"front matter is stripped (tags do not leak into body) and title becomes 'Stub Note'",
		fmt.Sprintf("leakedTags=%v, <title>=%q", leaked, gotEOFTitle),
		!leaked && strings.Contains(gotEOFTitle, "Stub Note"))
}

func attackHeadingSlugs(workDir string) {
	// MD-02a: complex GitHub heading slugs (inline code, links, emphasis, HTML entity, unicode).
	outA, resA, err := buildFixture(workDir, "slug-complex", map[string]string{
		"README.md": "# Book\n",
		"ch01-a/ch01-a.md": "# Chapter 1\n\n" +
			"## 5.1 The `internal/dbfile` package\n\n" +
			"## **Bold** &amp; _italic_ — [link](https://sqlite.org/)\n\n" +
			"## Summary\n\n## Summary\n\n## Summary-1\n\n" +
			"Jump: [a](#51-the-internaldbfile-package) [b](#bold--italic--link) [c](#summary) [d](#summary-1) [e](#summary-1-1).\n",
	})
	if err != nil {
		fatal("slug-complex build", err)
	}
	checkedA, err := site.CheckSite(outA, "/book/")
	if err != nil {
		fatal("slug-complex check", err)
	}
	heldA := len(resA.Problems) == 0 && len(checkedA.Problems) == 0
	report("MD-02a",
		"Render headings with code spans, bold/italic, HTML entities (&amp;), inline links, em-dashes, and colliding duplicates ('Summary', 'Summary', 'Summary-1')",
		"all GitHub-compatible heading anchors resolve cleanly in site.CheckSite",
		fmt.Sprintf("buildProblems=%d, checkProblems=%d (%v)", len(resA.Problems), len(checkedA.Problems), checkedA.Problems),
		heldA)

	// MD-02b: punctuation-only / emoji-only headings ('## ???' and '## !!!').
	outB, _, err := buildFixture(workDir, "slug-punct", map[string]string{
		"README.md":        "# Book\n",
		"ch01-a/ch01-a.md": "# Chapter 1\n\n## ???\n\nBody 1.\n\n## !!!\n\nBody 2.\n",
	})
	if err != nil {
		fatal("slug-punct build", err)
	}
	chHTML := readOut(outB, "ch01-a/index.html")
	emptyID := strings.Contains(chHTML, `<h2 id="">???</h2>`)
	emptyTOCHref := strings.Contains(chHTML, `<a href="#">???</a>`)
	checkedB, err := site.CheckSite(outB, "/book/")
	if err != nil {
		fatal("slug-punct check", err)
	}
	report("MD-02b",
		"Render punctuation-only headings '## ???' and '## !!!'",
		"headings do not produce empty id=\"\" or broken TOC href=\"#\"",
		fmt.Sprintf("emptyID=%v, emptyTOCHref=%v, checkProblems=%d", emptyID, emptyTOCHref, len(checkedB.Problems)),
		!emptyID && !emptyTOCHref)
}

func attackIncludeMarkers(workDir string) {
	out, res, err := buildFixture(workDir, "includes", map[string]string{
		"README.md": "# Book\n",
		"ch01-a/ch01-a.md": "# Chapter 1\n\n## 1. Standard\n\n" +
			"<!--   include:   experiments/1.1-a/main.go   -->\n```go\npackage main\n```\n\n" +
			"Paragraph directly before include without blank line.\n" +
			"<!-- include: experiments/1.1-a/main.go -->\n```go\npackage main\n```\n\n" +
			"- List item with indented include:\n\n" +
			"  <!-- include: experiments/1.1-a/main.go -->\n\n" +
			"<!-- adversarial-results -->\n\n" +
			"Fenced code showing a literal include comment:\n\n" +
			"```markdown\n<!-- include: experiments/1.1-a/main.go -->\n```\n",
		"ch01-a/experiments/1.1-a/main.go": "package main\n",
	})
	if err != nil {
		fatal("includes build", err)
	}
	chHTML := readOut(out, "ch01-a/index.html")
	includeCount := strings.Count(chHTML, `<p class="include"><a href="https://github.com/example/book/blob/main/ch01-a/experiments/1.1-a/main.go">ch01-a/experiments/1.1-a/main.go</a></p>`)
	keptOtherComment := strings.Contains(chHTML, `<!-- adversarial-results -->`)
	keptLiteralCode := strings.Contains(chHTML, `&lt;!-- include: experiments/1.1-a/main.go --&gt;`)
	// All 3 real include markers (extra spaces, directly after paragraph, inside list item) should convert;
	// non-include comment stays a comment; fenced literal stays escaped code.
	held := len(res.Problems) == 0 && includeCount == 3 && keptOtherComment && keptLiteralCode
	report("MD-03",
		"Test include markers: extra whitespace, immediately after paragraph, indented inside list item, non-include HTML comment, and literal marker inside fenced code",
		"3 real include markers convert to <p class=\"include\">, non-include comment stays comment, fenced code marker stays literal escaped text",
		fmt.Sprintf("converted=%d/3, keptOtherComment=%v, keptLiteralInCode=%v, problems=%d", includeCount, keptOtherComment, keptLiteralCode, len(res.Problems)),
		held)
}

func attackExperimentDiscovery(workDir string) {
	// MD-04a: 10 experiments (1.1..1.10) numeric sort, 1.1 vs 1.10 prefix, mention heading ignored,
	// code-only / evidence-only / both Code · Evidence separator handling.
	files := map[string]string{
		"README.md": "# Book\n",
		"ch01-a/ch01-a.md": "# Chapter 1\n\n## 1. Labs\n\n" +
			"### Experiment 1.1 — First (`1.1-first`)\n\nText.\n\n" +
			"### Notes on Experiment 1.1\n\nMention only.\n\n" +
			"### 5.2 Experiment 1.2 — Code only (`1.2-codeonly`)\n\nText.\n\n" +
			"### 5.3 Experiment 1.3 — Evidence only (`1.3-evonly`)\n\nText.\n\n" +
			"### 5.10 Experiment 1.10 — Tenth (`1.10-tenth`)\n\nText.\n",
		"ch01-a/experiments/1.1-first/main.go":    "package main\n",
		"ch01-a/evidence/1.1-first.txt":           "ok\n",
		"ch01-a/experiments/1.2-codeonly/main.go": "package main\n",
		"ch01-a/experiments/1.3-evonly/helper.go": "package helper\n",
		"ch01-a/evidence/1.3-evonly.txt":          "ok\n",
		"ch01-a/experiments/1.10-tenth/main.go":   "package main\n",
		"ch01-a/evidence/1.10-tenth.txt":          "ok\n",
	}
	outA, resA, err := buildFixture(workDir, "exp-order", files)
	if err != nil {
		fatal("exp-order build", err)
	}
	homeA := readOut(outA, "index.html")
	chA := readOut(outA, "ch01-a/index.html")

	idx1 := strings.Index(homeA, "1.1-first")
	idx2 := strings.Index(homeA, "1.2-codeonly")
	idx3 := strings.Index(homeA, "1.3-evonly")
	idx10 := strings.Index(homeA, "1.10-tenth")
	orderOK := idx1 >= 0 && idx1 < idx2 && idx2 < idx3 && idx3 < idx10

	// 1.1 has Code · Evidence; 1.2 has Code only (no '·'); 1.3 has Evidence only (no '·'); 1.10 has Code · Evidence.
	has1Both := strings.Contains(chA, `<p class="experiment-links"><a href="https://github.com/example/book/blob/main/ch01-a/experiments/1.1-first/main.go">Code</a> · <a href="https://github.com/example/book/blob/main/ch01-a/evidence/1.1-first.txt">Evidence</a></p>`)
	has2Code := strings.Contains(chA, `<p class="experiment-links"><a href="https://github.com/example/book/blob/main/ch01-a/experiments/1.2-codeonly/main.go">Code</a></p>`)
	has3Ev := strings.Contains(chA, `<p class="experiment-links"><a href="https://github.com/example/book/blob/main/ch01-a/evidence/1.3-evonly.txt">Evidence</a></p>`)
	has10Both := strings.Contains(chA, `<p class="experiment-links"><a href="https://github.com/example/book/blob/main/ch01-a/experiments/1.10-tenth/main.go">Code</a> · <a href="https://github.com/example/book/blob/main/ch01-a/evidence/1.10-tenth.txt">Evidence</a></p>`)
	linkLines := strings.Count(chA, `class="experiment-links"`)

	heldA := len(resA.Problems) == 0 && orderOK && has1Both && has2Code && has3Ev && has10Both && linkLines == 4
	report("MD-04a",
		"Test experiments 1.1, 1.2 (code-only), 1.3 (evidence-only), 1.10 (numeric sort vs 1.1 prefix), plus a mention heading '### Notes on Experiment 1.1'",
		"numeric sort 1.1 < 1.2 < 1.3 < 1.10; 4 Code/Evidence lines with no dangling separator; mention heading ignored",
		fmt.Sprintf("orderOK=%v, 1both=%v, 2code=%v, 3ev=%v, 10both=%v, totalLines=%d", orderOK, has1Both, has2Code, has3Ev, has10Both, linkLines),
		heldA)

	// MD-04b: Non-experiment helper directory under experiments/ containing a hyphen
	// (e.g. experiments/shared-helper/util.go). Spec says experiments are N.M-*.
	filesB := map[string]string{
		"README.md":                                  "# Book\n",
		"ch01-a/ch01-a.md":                           "# Chapter 1\n\n## 1. Lab\n\n### Experiment 1.1 — First (`1.1-first`)\n\nText.\n",
		"ch01-a/experiments/1.1-first/main.go":       "package main\n",
		"ch01-a/experiments/shared-helper/helper.go": "package sharedhelper\n",
	}
	outB, _, err := buildFixture(workDir, "exp-helper-dir", filesB)
	if err != nil {
		fatal("exp-helper-dir build", err)
	}
	homeB := readOut(outB, "index.html")
	falseListed := strings.Contains(homeB, "shared-helper")
	report("MD-04b",
		"Place a non-experiment helper directory ch01-a/experiments/shared-helper/helper.go alongside experiments/1.1-first/",
		"only N.M-* experiment directories are listed in the Reading path (shared-helper is ignored)",
		fmt.Sprintf("shared-helper listed in Reading path=%v", falseListed),
		!falseListed)
}

func attackRungDetection(workDir string) {
	// MD-05a: Chapter 3 references an earlier chapter's "Rebuild Ladder (rung R0)" before declaring its own rung R2.
	outA, _, err := buildFixture(workDir, "rung-backref", map[string]string{
		"README.md": "# Book\n",
		"ch03-btrees/ch03-btrees.md": "# Chapter 3 — B-trees\n\n" +
			"Chapter 1 introduced the Rebuild Ladder (rung R0) with **R0 (Recipe)**.\n\n" +
			"**The Rebuild Ladder (rung R2).** This chapter builds **R2 (Canonical form)**.\n",
	})
	if err != nil {
		fatal("rung-backref build", err)
	}
	homeA := readOut(outA, "index.html")
	gotRungA := extractBetween(homeA, `<span class="rung">`, `</span>`)
	report("MD-05a",
		"Chapter mentions earlier 'Rebuild Ladder (rung R0)' before declaring '**The Rebuild Ladder (rung R2).** ... **R2 (Canonical form)**'",
		"chapter is attributed to Rung R2 (Canonical form), not back-referenced R0",
		fmt.Sprintf("detected rung=%q", gotRungA),
		gotRungA == "Rung R2 (Canonical form)")

	// MD-05b: No-rung chapter (like ch04 or ch07) mentions "Rebuild Ladder (rung R0)" inside a fenced code block.
	outB, _, err := buildFixture(workDir, "rung-codeblock", map[string]string{
		"README.md": "# Book\n",
		"ch04-pager/ch04-pager.md": "# Chapter 4 — Pager\n\nThis chapter adds no rung.\n\n" +
			"```go\n// Uses Rebuild Ladder (rung R0) fixture\npackage main\n```\n",
	})
	if err != nil {
		fatal("rung-codeblock build", err)
	}
	homeB := readOut(outB, "index.html")
	gotRungB := extractBetween(homeB, `<span class="rung">`, `</span>`)
	report("MD-05b",
		"No-rung chapter contains 'Rebuild Ladder (rung R0)' inside a Go code block comment",
		"no rung is assigned to a chapter whose only mention is inside a fenced code block",
		fmt.Sprintf("detected rung=%q", gotRungB),
		gotRungB == "")
}

func attackGFMAndHTMLEscaping(workDir string) {
	// MD-06: GFM tables, strikethrough, task lists, autolinks, and HTML-escaping of metacharacters in titles/headings.
	out, res, err := buildFixture(workDir, "gfm-xss", map[string]string{
		"README.md": "# Book <script> & \"Quotes\"\n\nAutolink: https://sqlite.org/wal.html\n",
		"ch01-a/ch01-a.md": "# Chapter 1 — <img src=x onerror=alert(1)> & `code`\n\n" +
			"## 1. GFM Features\n\n" +
			"| Col A | Col B |\n| --- | --- |\n| ~~old~~ | **new** |\n\n" +
			"- [x] Done task\n- [ ] Open task\n\n" +
			"## 2. Second <script> Heading\n\nText.\n",
	})
	if err != nil {
		fatal("gfm-xss build", err)
	}
	home := readOut(out, "index.html")
	ch := readOut(out, "ch01-a/index.html")

	hasTable := strings.Contains(ch, "<table>") && strings.Contains(ch, "<del>old</del>")
	hasTasks := strings.Contains(ch, `type="checkbox"`)
	hasAuto := strings.Contains(home, `<a href="https://sqlite.org/wal.html">https://sqlite.org/wal.html</a>`)
	// Chrome (title, header, reading-path, TOC) uses html/template and must escape <script> / <img...>.
	// Note: goldmark with html.WithUnsafe() passes raw HTML in body headings, while TOC/title/header
	// extract plain text (skipping RawHTML) or HTML-escape via html/template.
	rawScriptInHead := strings.Contains(extractBetween(home, "<head>", "</head>"), "<script>")
	rawScriptInHeader := strings.Contains(extractBetween(home, "<header", "</header>"), "<script>")
	rawImgInTitle := strings.Contains(extractBetween(ch, "<title>", "</title>"), "<img")
	rawScriptInTOC := strings.Contains(extractBetween(ch, `<nav class="toc"`, `</nav>`), "<script>")

	held := len(res.Problems) == 0 && hasTable && hasTasks && hasAuto &&
		!rawScriptInHead && !rawScriptInHeader && !rawImgInTitle && !rawScriptInTOC
	report("MD-06",
		"Render GFM tables, strikethrough, task lists, autolinks, and HTML metacharacters (<script>, <img onerror>, &, \") in book/chapter titles",
		"all GFM features render and chrome (<head>, <header>, <title>, TOC) escapes HTML metacharacters cleanly",
		fmt.Sprintf("table+del=%v, tasks=%v, autolink=%v, unescapedInChrome=%v",
			hasTable, hasTasks, hasAuto, rawScriptInHead || rawScriptInHeader || rawImgInTitle || rawScriptInTOC),
		held)
}

func attackChapterTOCAnswersAndGitView(courseBin, workDir string) {
	// MD-07a: Chapter with 1 H2 heading AND answers.md -> does Answers link disappear from sidebar?
	outA, _, err := buildFixture(workDir, "short-ch-answers", map[string]string{
		"README.md":                "# Book\n",
		"ch01-short/ch01-short.md": "# Chapter 1 — Short\n\n## Only One Section\n\nBody.\n",
		"ch01-short/answers.md":    "# Chapter 1 Answers\n\nSecret answer.\n",
	})
	if err != nil {
		fatal("short-ch-answers build", err)
	}
	chShort := readOut(outA, "ch01-short/index.html")
	hasAnswersLink := strings.Contains(chShort, `/book/ch01-short/answers/`)
	hasInlineLeak := strings.Contains(chShort, "Secret answer.")
	report("MD-07a",
		"Build a chapter with only 1 H2 heading plus answers.md",
		"chapter page still surfaces the sidebar Answers link and never renders answers inline",
		fmt.Sprintf("hasAnswersLinkInChapterHTML=%v, answersLeakedInline=%v", hasAnswersLink, hasInlineLeak),
		hasAnswersLink && !hasInlineLeak)

	// MD-07b: 10 chapters (ch01..ch10) ordering, Previous/Next chain, missing README.md error handling.
	files10 := map[string]string{"README.md": "# 10-Chapter Book\n"}
	for i := 1; i <= 10; i++ {
		slug := fmt.Sprintf("ch%02d-part%d", i, i)
		files10[slug+"/"+slug+".md"] = fmt.Sprintf("# Chapter %d\n\n## 1. A\n\n## 2. B\n", i)
	}
	out10, res10, err := buildFixture(workDir, "ten-chapters", files10)
	if err != nil {
		fatal("ten-chapters build", err)
	}
	var chainIssues []string
	if len(res10.Problems) != 0 {
		chainIssues = append(chainIssues, fmt.Sprintf("problems=%d", len(res10.Problems)))
	}
	for i := 1; i <= 10; i++ {
		slug := fmt.Sprintf("ch%02d-part%d", i, i)
		p := readOut(out10, slug+"/index.html")
		if i == 1 && strings.Contains(p, `class="previous"`) {
			chainIssues = append(chainIssues, "ch01 has Previous")
		}
		if i > 1 {
			prevSlug := fmt.Sprintf("ch%02d-part%d", i-1, i-1)
			if !strings.Contains(p, `class="previous" href="/book/`+prevSlug+`/"`) {
				chainIssues = append(chainIssues, slug+" missing Previous -> "+prevSlug)
			}
		}
		if i < 10 {
			nextSlug := fmt.Sprintf("ch%02d-part%d", i+1, i+1)
			if !strings.Contains(p, `class="next" href="/book/`+nextSlug+`/"`) {
				chainIssues = append(chainIssues, slug+" missing Next -> "+nextSlug)
			}
		}
		if i == 10 && strings.Contains(p, `class="next"`) {
			chainIssues = append(chainIssues, "ch10 has Next")
		}
	}
	// Missing README.md must return an error without panicking.
	_, _, noReadmeErr := buildFixture(workDir, "no-readme", map[string]string{
		"ch01-a/ch01-a.md": "# Chapter 1\n",
	})
	if noReadmeErr == nil {
		chainIssues = append(chainIssues, "missing README.md did not error")
	}
	report("MD-07b",
		"Build a 10-chapter book (ch01..ch10) to verify Previous/Next chain & numeric order, plus missing README.md handling",
		"ch01..ch10 link in exact sequence with no Previous on ch01 / Next on ch10; missing README.md returns error",
		func() string {
			if len(chainIssues) == 0 {
				return "all 10 chapters chained cleanly; missing README.md returned error: " + noReadmeErr.Error()
			}
			return strings.Join(chainIssues, "; ")
		}(),
		len(chainIssues) == 0)

	// MD-07c: Verify cmd/course site file discovery: includes untracked non-ignored chapter,
	// excludes gitignored chapter, excludes tracked-but-deleted-on-disk file.
	gitRepo := filepath.Join(workDir, "git-view-repo")
	if err := os.RemoveAll(gitRepo); err != nil {
		fatal("clean git-view-repo", err)
	}
	if err := os.MkdirAll(gitRepo, 0o755); err != nil {
		fatal("mkdir git-view-repo", err)
	}
	mustGit(gitRepo, "init", "-q", "-b", "main")
	mustGit(gitRepo, "config", "user.name", "Adv Test")
	mustGit(gitRepo, "config", "user.email", "adv@example.invalid")
	writeFile(filepath.Join(gitRepo, "go.mod"), "module "+sqlitex.ModulePath+"\n\ngo 1.26.0\n")
	writeFile(filepath.Join(gitRepo, ".gitignore"), "/ch99-ignored/\n")
	writeFile(filepath.Join(gitRepo, "README.md"), "# Git View\n\nSee [ch1](ch01-tracked/ch01-tracked.md).\n")
	writeFile(filepath.Join(gitRepo, "ch01-tracked/ch01-tracked.md"), "# Ch1 Tracked\n\n## A\n\n## B\n")
	writeFile(filepath.Join(gitRepo, "ch03-deleted/ch03-deleted.md"), "# Ch3 Deleted\n")
	mustGit(gitRepo, "add", "-A")
	mustGit(gitRepo, "commit", "-q", "-m", "init")
	// Add untracked non-ignored ch02, gitignored ch99, and delete tracked ch03 from disk.
	writeFile(filepath.Join(gitRepo, "ch02-untracked/ch02-untracked.md"), "# Ch2 Untracked\n\n## A\n\n## B\n")
	writeFile(filepath.Join(gitRepo, "ch99-ignored/ch99-ignored.md"), "# Ch99 Ignored\n")
	if err := os.Remove(filepath.Join(gitRepo, "ch03-deleted/ch03-deleted.md")); err != nil {
		fatal("remove ch03-deleted", err)
	}

	gitOut := filepath.Join(workDir, "git-view-out")
	code, out, _ := runCmd(gitRepo, courseBin, "site", "-out", gitOut, "-base", "/book/")
	_, errCh1 := os.Stat(filepath.Join(gitOut, "ch01-tracked/index.html"))
	_, errCh2 := os.Stat(filepath.Join(gitOut, "ch02-untracked/index.html"))
	_, errCh3 := os.Stat(filepath.Join(gitOut, "ch03-deleted/index.html"))
	_, errCh99 := os.Stat(filepath.Join(gitOut, "ch99-ignored/index.html"))
	heldGit := code == 0 && errCh1 == nil && errCh2 == nil && os.IsNotExist(errCh3) && os.IsNotExist(errCh99)
	report("MD-07c",
		"Build via cmd/course site with tracked ch01, untracked non-ignored ch02, tracked-but-deleted-on-disk ch03, and gitignored ch99",
		"ch01 and ch02 are rendered; deleted ch03 and gitignored ch99 are excluded",
		fmt.Sprintf("exit=%d, ch01=%v, ch02=%v, ch03Absent=%v, ch99Absent=%v (%s)",
			code, errCh1 == nil, errCh2 == nil, os.IsNotExist(errCh3), os.IsNotExist(errCh99), strings.TrimSpace(out)),
		heldGit)
}

func extractBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
