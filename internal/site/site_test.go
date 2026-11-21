package site

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const testRepoURL = "https://github.com/example/book"

func fixture() map[string]string {
	return map[string]string{
		"README.md": "# The Book\n\nStart with [chapter one](ch01-alpha/ch01-alpha.md) or the [plan](OUTLINE.md).\n" +
			"See [state](STATE.md) and [notes](docs/knowledge/).\n",
		"STATE.md":   "# State\n",
		"OUTLINE.md": "# Outline\n\n## 1. First part\n\nJump to [rulings](#2-rulings-x-2026).\n\n## 2. Rulings (X, 2026)\n",
		"ch01-alpha/ch01-alpha.md": "# Chapter 1 — Alpha\n\n**The Rebuild Ladder (rung R0).** This builds rung **R0 (Recipe)** here.\n\n" +
			"## 1. Model\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n" +
			"### Experiment 1.1 — Pin check (`1.1-pin`)\n\n" +
			"<!-- include: experiments/1.1-pin/main.go -->\n```go\npackage main\n```\n\n" +
			"Read [the census](../ch02-beta/ch02-beta.md#2-census) and [code](../internal/x/x.go#L3) and [dir](../internal/x/).\n\n" +
			"### Determinism rules verified by Experiment 1.1\n\n" +
			"## 8. Knowledge check\n\nAnswers are in [answers.md](answers.md). Back to [Model](#1-model).\n",
		"ch01-alpha/answers.md":                    "# Chapter 1 — Answers\n\n1. Yes.\n",
		"ch01-alpha/experiments/1.1-pin/main.go":   "package main\n",
		"ch01-alpha/evidence/1.1-pin.txt":          "ok\n",
		"ch01-alpha/experiments/1.2-extra/main.go": "package main\n",
		"ch02-beta/ch02-beta.md":                   "# Chapter 2 — Beta\n\n## 1. Model\n\n## 2. Census\n\n### 5.4 Experiment 2.1 — Census (`2.1-census`)\n\nBack to [one](../ch01-alpha/).\n",
		"ch02-beta/experiments/2.1-census/main.go": "package main\n",
		"internal/x/x.go":                          "package x\n",
		"docs/knowledge/note.md":                   "---\ntitle: \"Front title\"\ntags: [a]\n---\n\n# Note heading\n\nBody text.\n",
		"docs/knowledge/bare.md":                   "---\ntitle: Bare title\n---\n\nNo heading here.\n",
	}
}

func writeFixture(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()
	root := t.TempDir()
	var names []string
	for name, content := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return root, names
}

func build(t *testing.T, files map[string]string, base string) (string, Result) {
	t.Helper()
	root, names := writeFixture(t, files)
	out := t.TempDir()
	result, err := Build(Options{Root: root, Files: names, Base: base, RepoURL: testRepoURL, Branch: "main"}, out)
	if err != nil {
		t.Fatal(err)
	}
	return out, result
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustContain(t *testing.T, page, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Errorf("%s: missing %q", page, needle)
		}
	}
}

func TestBuildLayoutAndCheck(t *testing.T) {
	out, result := build(t, fixture(), "/book/")
	if len(result.Problems) != 0 {
		t.Fatalf("problems: %v", result.Problems)
	}
	want := []string{
		"index.html",
		"ch01-alpha/index.html",
		"ch01-alpha/answers/index.html",
		"ch02-beta/index.html",
		"outline/index.html",
		"knowledge/bare/index.html",
		"knowledge/note/index.html",
	}
	if strings.Join(result.Pages, ",") != strings.Join(want, ",") {
		t.Fatalf("pages = %v, want %v", result.Pages, want)
	}
	for _, name := range []string{".nojekyll", "style.css"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatal(err)
		}
	}
	checked, err := CheckSite(out, "/book/")
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.Problems) != 0 {
		t.Fatalf("check problems: %v", checked.Problems)
	}
	if checked.Pages != len(want) || checked.Links == 0 {
		t.Fatalf("checked %d pages, %d links", checked.Pages, checked.Links)
	}
}

func TestHomePage(t *testing.T) {
	out, _ := build(t, fixture(), "/book/")
	home := read(t, out, "index.html")
	mustContain(t, "home", home,
		`<a href="/book/ch01-alpha/">chapter one</a>`,
		`<a href="/book/outline/">plan</a>`,
		`href="`+testRepoURL+`/blob/main/STATE.md"`,
		`href="`+testRepoURL+`/tree/main/docs/knowledge"`,
		`<h2 id="reading-path">Reading path</h2>`,
		`<span class="rung">Rung R0 (Recipe)</span>`,
		`<a href="/book/ch01-alpha/#experiment-11--pin-check-11-pin">Experiment 1.1 — Pin check (1.1-pin)</a>`,
		`<a href="/book/ch01-alpha/">1.2-extra</a>`,
		`<a href="/book/ch02-beta/#54-experiment-21--census-21-census">Experiment 2.1 — Census (2.1-census)</a>`,
		`<h2 id="reference">Reference</h2>`,
		`<a href="/book/outline/">How this book was planned</a>`,
		`<a href="/book/knowledge/note/">Note heading</a>`,
		`<a href="/book/knowledge/bare/">Bare title</a>`,
		`<link rel="stylesheet" href="/book/style.css">`,
	)
	if strings.Contains(home, `rung">Rung R`) && strings.Count(home, `class="rung"`) != 1 {
		t.Errorf("only chapter 1 names a rung")
	}
	if strings.Index(home, "1.1-pin)</a>") > strings.Index(home, ">1.2-extra<") {
		t.Errorf("experiments out of order")
	}
}

func TestChapterPage(t *testing.T) {
	out, _ := build(t, fixture(), "/book/")
	page := read(t, out, "ch01-alpha/index.html")
	mustContain(t, "chapter", page,
		`<nav class="toc" aria-label="Contents">`,
		`<li class="toc-2"><a href="#1-model">1. Model</a></li>`,
		`<li class="toc-3"><a href="#experiment-11--pin-check-11-pin">`,
		`<p class="toc-answers"><a href="/book/ch01-alpha/answers/">Answers</a></p>`,
		`<a class="next" href="/book/ch02-beta/"><span>Next</span> Chapter 2 — Beta</a>`,
		`<p class="experiment-links"><a href="`+testRepoURL+`/blob/main/ch01-alpha/experiments/1.1-pin/main.go">Code</a> · <a href="`+testRepoURL+`/blob/main/ch01-alpha/evidence/1.1-pin.txt">Evidence</a></p>`,
		`<p class="include"><a href="`+testRepoURL+`/blob/main/ch01-alpha/experiments/1.1-pin/main.go">ch01-alpha/experiments/1.1-pin/main.go</a></p>`,
		`<a href="/book/ch02-beta/#2-census">the census</a>`,
		`<a href="`+testRepoURL+`/blob/main/internal/x/x.go#L3">code</a>`,
		`<a href="`+testRepoURL+`/tree/main/internal/x">dir</a>`,
		`<a href="/book/ch01-alpha/answers/">answers.md</a>`,
		`<a href="#1-model">Model</a>`,
		`<table>`,
	)
	if strings.Contains(page, `class="previous"`) {
		t.Errorf("first chapter has a previous link")
	}
	if strings.Count(page, `class="experiment-links"`) != 1 {
		t.Errorf("experiment links must follow only the experiment heading")
	}
	if strings.Contains(page, "<!-- include") {
		t.Errorf("include marker left in output")
	}
	if strings.Contains(page, "Yes.") {
		t.Errorf("answers rendered inline")
	}
	second := read(t, out, "ch02-beta/index.html")
	mustContain(t, "chapter 2", second,
		`<a class="previous" href="/book/ch01-alpha/"><span>Previous</span> Chapter 1 — Alpha</a>`,
		`<a href="/book/ch01-alpha/">one</a>`,
		`<p class="experiment-links"><a href="`+testRepoURL+`/blob/main/ch02-beta/experiments/2.1-census/main.go">Code</a></p>`,
	)
	answers := read(t, out, "ch01-alpha/answers/index.html")
	mustContain(t, "answers", answers, `<p class="chapter-link"><a href="/book/ch01-alpha/">Chapter 1 — Alpha</a></p>`, "Yes.")
}

func TestKnowledgeAndOutlinePages(t *testing.T) {
	out, _ := build(t, fixture(), "/")
	note := read(t, out, "knowledge/note/index.html")
	if strings.Contains(note, "tags:") || strings.Contains(note, "Front title") {
		t.Errorf("front matter leaked into the page")
	}
	mustContain(t, "note", note, "<title>Note heading · The Book</title>", "Body text.")
	bare := read(t, out, "knowledge/bare/index.html")
	mustContain(t, "bare", bare, "<title>Bare title · The Book</title>")
	outline := read(t, out, "outline/index.html")
	mustContain(t, "outline", outline, `<a href="#2-rulings-x-2026">rulings</a>`, `<h2 id="2-rulings-x-2026">`)
}

func TestDeterministicOutput(t *testing.T) {
	root, names := writeFixture(t, fixture())
	opts := Options{Root: root, Files: names, Base: "/book/", RepoURL: testRepoURL, Branch: "main"}
	first, second := t.TempDir(), t.TempDir()
	if _, err := Build(opts, first); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(opts, second); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(first, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(first, name)
		a, _ := os.ReadFile(name)
		b, err := os.ReadFile(filepath.Join(second, rel))
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs between builds", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBrokenLinksReported(t *testing.T) {
	files := fixture()
	files["ch02-beta/ch02-beta.md"] += "\n[gone](missing.go) [out](../../etc/passwd) [abs](/x) [anchor](../ch01-alpha/ch01-alpha.md#no-such-anchor)\n"
	out, result := build(t, files, "/book/")
	var messages []string
	for _, p := range result.Problems {
		messages = append(messages, p.String())
	}
	joined := strings.Join(messages, "\n")
	for _, want := range []string{"missing.go", "leaves the repository", "absolute link /x"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q lack %q", joined, want)
		}
	}
	checked, err := CheckSite(out, "/book/")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range checked.Problems {
		if p.Where == "ch02-beta/index.html" && strings.Contains(p.Message, "missing anchor #no-such-anchor") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing anchor not caught: %v", checked.Problems)
	}
}

func TestCheckSiteFlagsBrokenPagesAndBase(t *testing.T) {
	dir := t.TempDir()
	pages := map[string]string{
		"index.html":   `<a href="/book/a/">a</a> <a href="/book/a/#here">h</a> <a href="/other/">o</a> <a href="/book/nope/">n</a> <a href="#top">t</a> <a href="https://x.test/">x</a>`,
		"a/index.html": `<h2 id="here">x</h2><a href="../index.html#top">u</a><a href="/book/style.css">s</a>`,
	}
	for name, body := range pages {
		target := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(target), 0o755)
		os.WriteFile(target, []byte(body), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "style.css"), nil, 0o644)
	checked, err := CheckSite(dir, "book")
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, p := range checked.Problems {
		messages = append(messages, p.String())
	}
	want := []string{
		"a/index.html: missing anchor #top in ../index.html#top",
		"index.html: link outside the site base /book/: /other/",
		"index.html: broken link /book/nope/",
		"index.html: missing anchor #top in #top",
	}
	if strings.Join(messages, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(messages, "\n"), strings.Join(want, "\n"))
	}
	if checked.Links != 8 {
		t.Fatalf("links = %d", checked.Links)
	}
}

func TestGithubSlugs(t *testing.T) {
	s := newSlugger()
	cases := []struct{ in, want string }{
		{"8. Phase 2 rulings (Nick, 2026-11-20)", "8-phase-2-rulings-nick-2026-11-20"},
		{"Chapter 1 — Architecture and execution path", "chapter-1--architecture-and-execution-path"},
		{"Reading the evidence", "reading-the-evidence"},
		{"Reading the evidence", "reading-the-evidence-1"},
		{"Reading the evidence", "reading-the-evidence-2"},
		{"Reading the evidence-1", "reading-the-evidence-1-1"},
		{"5.1 The internal/dbfile package", "51-the-internaldbfile-package"},
		{"Über_Größe café", "über_größe-café"},
	}
	for _, c := range cases {
		if got := s.slug(c.in); got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeBaseAndFindRung(t *testing.T) {
	for in, want := range map[string]string{"": "/", "/": "/", "x": "/x/", "/x": "/x/", "/x/": "/x/", "a/b": "/a/b/"} {
		if got := NormalizeBase(in); got != want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
	if got := findRung([]byte("rung R4 later. The Rebuild Ladder (rung R1). builds **R1 (Pages)**")); got != "R1 (Pages)" {
		t.Errorf("rung = %q", got)
	}
	if got := findRung([]byte("The Rebuild Ladder (rung R7).")); got != "R7" {
		t.Errorf("rung = %q", got)
	}
	if got := findRung([]byte("no ladder")); got != "" {
		t.Errorf("rung = %q", got)
	}
}

func TestStripFrontMatter(t *testing.T) {
	body, title := stripFrontMatter([]byte("---\ntitle: 'Quoted'\n---\n# H\n"))
	if string(body) != "# H\n" || title != "Quoted" {
		t.Fatalf("body %q title %q", body, title)
	}
	body, title = stripFrontMatter([]byte("---\nunterminated\n"))
	if string(body) != "---\nunterminated\n" || title != "" {
		t.Fatalf("body %q title %q", body, title)
	}
}

func TestStripFrontMatterEdges(t *testing.T) {
	cases := []struct{ in, body, title string }{
		{"---\n---\n# Real\n\nPara.\n\n---\n\nAfter.\n", "# Real\n\nPara.\n\n---\n\nAfter.\n", ""},
		{"---\ntitle: Stub Note\ntags: [stub]\n---", "", "Stub Note"},
		{"---\ntitle: \"SQLite's 'WAL'\"\n---\nx\n", "x\n", "SQLite's 'WAL'"},
		{"---\ntitle: 'It''s'\n---\nx\n", "x\n", "It's"},
	}
	for _, c := range cases {
		body, title := stripFrontMatter([]byte(c.in))
		if string(body) != c.body || title != c.title {
			t.Errorf("stripFrontMatter(%q) = %q, %q; want %q, %q", c.in, body, title, c.body, c.title)
		}
	}
}

func TestFrontMatterOnlyOnKnowledgePages(t *testing.T) {
	out, _ := build(t, map[string]string{"README.md": "---\n# Book Title\n\nSome intro\n---\n\nBody after rule\n"}, "/")
	mustContain(t, "index.html", read(t, out, "index.html"), "Book Title", "Some intro", "Body after rule")
}

func TestRungIgnoresCodeAndPrefersDeclaration(t *testing.T) {
	out, _ := build(t, map[string]string{
		"README.md": "# Book\n",
		"ch03-b/ch03-b.md": "# Three\n\nChapter 1 introduced the Rebuild Ladder (rung R0) with **R0 (Recipe)**.\n\n" +
			"**The Rebuild Ladder (rung R2).** This chapter builds **R2 (Canonical form)**.\n",
		"ch04-p/ch04-p.md": "# Four\n\nNo rung.\n\n```go\n// Rebuild Ladder (rung R0)\n```\n\nAnd `Rebuild Ladder (rung R1)` in code.\n",
	}, "/")
	home := read(t, out, "index.html")
	mustContain(t, "index.html", home, `<span class="rung">Rung R2 (Canonical form)</span>`)
	if strings.Count(home, `class="rung"`) != 1 {
		t.Fatalf("rungs: %s", home)
	}
}

func TestOnlyNumberedExperimentDirectoriesAreListed(t *testing.T) {
	out, _ := build(t, map[string]string{
		"README.md":                         "# Book\n",
		"ch01-a/ch01-a.md":                  "# One\n",
		"ch01-a/experiments/1.1-x/main.go":  "package main\n",
		"ch01-a/experiments/shared-h/h.go":  "package h\n",
		"ch01-a/experiments/notes-1.2/x.go": "package x\n",
	}, "/")
	home := read(t, out, "index.html")
	mustContain(t, "index.html", home, "1.1-x")
	if strings.Contains(home, "shared-h") || strings.Contains(home, "notes-1.2") {
		t.Fatalf("helper directories listed: %s", home)
	}
}

func TestAnswersLinkWithoutTableOfContents(t *testing.T) {
	out, _ := build(t, map[string]string{
		"README.md":         "# Book\n",
		"ch01-s/ch01-s.md":  "# One\n\n## Only section\n",
		"ch01-s/answers.md": "# Answers\n\nSecret.\n",
	}, "/b/")
	page := read(t, out, "ch01-s/index.html")
	mustContain(t, "ch01-s/index.html", page, `<a href="/b/ch01-s/answers/">Answers</a>`)
	if strings.Contains(page, "Secret.") || strings.Contains(page, "<h2>Contents</h2>") {
		t.Fatalf("page: %s", page)
	}
}

func TestHeadingIDsFromEntitiesAndPunctuation(t *testing.T) {
	out, result := build(t, map[string]string{
		"README.md": "# Book\n",
		"ch01-a/ch01-a.md": "# One\n\n## **Bold** &amp; _italic_ — [link](https://sqlite.org/)\n\n## `a &amp; b`\n\n## ???\n\n## !!!\n\n" +
			"[x](#bold--italic--link) [y](#a-amp-b) [z](#section) [w](#section-1)\n",
	}, "/")
	if len(result.Problems) != 0 {
		t.Fatal(result.Problems)
	}
	page := read(t, out, "ch01-a/index.html")
	if strings.Contains(page, `id=""`) || strings.Contains(page, `href="#"`) {
		t.Fatalf("empty id or href: %s", page)
	}
	checked, err := CheckSite(out, "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.Problems) != 0 {
		t.Fatal(checked.Problems)
	}
}

func TestIncludeMarkerWithLineRange(t *testing.T) {
	out, _ := build(t, map[string]string{
		"README.md":        "# Book\n",
		"ch01-a/ch01-a.md": "# One\n\n<!-- include: foo.go lines=3-7 -->\n```go\nx\n```\n",
		"ch01-a/foo.go":    "package a\n",
	}, "/")
	mustContain(t, "ch01-a/index.html", read(t, out, "ch01-a/index.html"),
		`<p class="include"><a href="`+testRepoURL+`/blob/main/ch01-a/foo.go#L3-L7">ch01-a/foo.go:3-7</a></p>`)
}

func TestQueryStringsAndEscapesInRelativeLinks(t *testing.T) {
	out, result := build(t, map[string]string{
		"README.md":        "# Book\n",
		"ch01-a/ch01-a.md": "# One\n\n## 1. Model\n",
		"ch02-b/ch02-b.md": "# Two\n\n[a](../ch01-a/ch01-a.md?v=1#1-model) [b](../internal/x.go?plain=1#L2) [c](../internal/a\\_b.go)\n",
		"internal/x.go":    "package internal\n",
		"internal/a_b.go":  "package internal\n",
	}, "/")
	if len(result.Problems) != 0 {
		t.Fatal(result.Problems)
	}
	mustContain(t, "ch02-b/index.html", read(t, out, "ch02-b/index.html"),
		`href="/ch01-a/#1-model"`, `href="`+testRepoURL+`/blob/main/internal/x.go?plain=1#L2"`, `href="`+testRepoURL+`/blob/main/internal/a_b.go"`)
}

func TestBuildRejectsAbsoluteFiles(t *testing.T) {
	if _, err := Build(Options{Root: t.TempDir(), Files: []string{"/etc/README.md"}}, t.TempDir()); err == nil {
		t.Fatal("absolute file accepted")
	}
}

func TestCheckSiteHTMLEdges(t *testing.T) {
	dir := t.TempDir()
	pages := map[string]string{
		"index.html": `<a href='/b/missing-single/'>x</a><img src='/b/missing.png'>` +
			`<!-- <a href="/b/in-comment/">c</a> id="ghost" -->` +
			`<a href="#ghost">g</a><a href="/b//a/#nope">d</a><a href="/b/a/?q=1#here">q</a>` +
			`<div id="dup"></div><h2 id='dup'>t</h2>`,
		"a/index.html": `<h2 id="here">h</h2>`,
	}
	for name, content := range pages {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checked, err := CheckSite(dir, "/b/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range checked.Problems {
		got = append(got, p.String())
	}
	want := []string{
		`index.html: duplicate id "dup"`,
		"index.html: broken link /b/missing-single/",
		"index.html: broken link /b/missing.png",
		"index.html: missing anchor #ghost in #ghost",
		"index.html: missing anchor #nope in /b//a/#nope",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
