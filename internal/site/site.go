// Package site renders the book repository into a self-contained static HTML
// site for GitHub Pages and verifies every internal link, anchor, and DOM id in
// the generated output.
package site

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark/util"
)

// Options configures a static site build for a repository checkout.
type Options struct {
	Root    string
	Files   []string
	Base    string
	RepoURL string
	Branch  string
}

// Problem describes a single link, anchor, or HTML check failure tied to a
// source markdown file or generated HTML page.
type Problem struct {
	Where   string
	Message string
}

// String formats p as "Where: Message", or just Message when Where is empty.
func (p Problem) String() string {
	if p.Where == "" {
		return p.Message
	}
	return p.Where + ": " + p.Message
}

// Result summarizes the HTML pages written and link problems found by Build.
type Result struct {
	Pages    []string
	Problems []Problem
}

type pageKind int

const (
	homePage pageKind = iota
	chapterPage
	answersPage
	outlinePage
	knowledgePage
)

type page struct {
	kind    pageKind
	source  string
	urlPath string
	title   string
	chapter *chapter
	doc     *document
}

type chapter struct {
	dir         string
	page        *page
	answers     *page
	rung        string
	experiments []experiment
}

type experiment struct {
	id      string
	dir     string
	heading string
	anchor  string
}

type builder struct {
	opts     Options
	files    map[string]bool
	dirs     map[string]bool
	pages    []*page
	bySource map[string]*page
	byDir    map[string]*page
	home     *page
	chapters []*chapter
	outline  *page
	notes    []*page
	problems []Problem
}

var chapterDirPattern = regexp.MustCompile(`^ch\d\d-[A-Za-z0-9._-]+$`)

// NormalizeBase converts a URL base path into canonical leading-and-trailing
// slash form ("/" for root, "/name/" for a subpath).
func NormalizeBase(base string) string {
	base = strings.Trim(base, "/")
	if base == "" {
		return "/"
	}
	return "/" + base + "/"
}

// Build renders README.md, every chNN-* chapter and its optional answers.md,
// OUTLINE.md, and docs/knowledge/*.md into outDir along with style.css and an
// empty .nojekyll marker for GitHub Pages.
func Build(opts Options, outDir string) (Result, error) {
	opts.Base = NormalizeBase(opts.Base)
	opts.RepoURL = strings.TrimRight(opts.RepoURL, "/")
	if opts.Branch == "" {
		opts.Branch = "main"
	}
	b := &builder{
		opts:     opts,
		files:    map[string]bool{},
		dirs:     map[string]bool{},
		bySource: map[string]*page{},
		byDir:    map[string]*page{},
	}
	for _, name := range opts.Files {
		name = path.Clean(filepath.ToSlash(name))
		// Reject absolute or parent-escaping entries so the path.Dir walk below
		// always terminates at "." and every link target stays inside opts.Root.
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return Result{}, fmt.Errorf("file %s is not a path relative to the repository root", name)
		}
		b.files[name] = true
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			b.dirs[dir] = true
		}
	}
	if err := b.discover(); err != nil {
		return Result{}, err
	}
	for _, p := range b.pages {
		if err := b.load(p); err != nil {
			return Result{}, err
		}
	}
	for _, c := range b.chapters {
		b.findExperiments(c)
	}
	var result Result
	for _, p := range b.pages {
		b.addExperimentLinks(p)
		b.rewriteLinks(p)
		html, err := renderMarkdown(p.doc)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", p.source, err)
		}
		out, err := b.renderPage(p, html)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", p.source, err)
		}
		target := filepath.Join(outDir, filepath.FromSlash(p.urlPath), "index.html")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(target, out, 0o644); err != nil {
			return Result{}, err
		}
		result.Pages = append(result.Pages, path.Join(p.urlPath, "index.html"))
	}
	if err := os.WriteFile(filepath.Join(outDir, "style.css"), styleCSS, 0o644); err != nil {
		return Result{}, err
	}
	// .nojekyll tells GitHub Pages to serve the static tree verbatim and also
	// marks outDir as a prior site build for cmd/course's directory safety check.
	if err := os.WriteFile(filepath.Join(outDir, ".nojekyll"), nil, 0o644); err != nil {
		return Result{}, err
	}
	result.Problems = b.problems
	return result, nil
}

func (b *builder) addPage(p *page) {
	b.pages = append(b.pages, p)
	b.bySource[p.source] = p
}

func (b *builder) discover() error {
	if !b.files["README.md"] {
		return errors.New("README.md is not in the file set")
	}
	b.home = &page{kind: homePage, source: "README.md", urlPath: ""}
	b.addPage(b.home)
	var dirs []string
	for dir := range b.dirs {
		if !strings.Contains(dir, "/") && chapterDirPattern.MatchString(dir) && b.files[dir+"/"+dir+".md"] {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		c := &chapter{dir: dir}
		c.page = &page{kind: chapterPage, source: dir + "/" + dir + ".md", urlPath: dir + "/", chapter: c}
		b.addPage(c.page)
		b.byDir[dir] = c.page
		if b.files[dir+"/answers.md"] {
			c.answers = &page{kind: answersPage, source: dir + "/answers.md", urlPath: dir + "/answers/", chapter: c}
			b.addPage(c.answers)
		}
		b.chapters = append(b.chapters, c)
	}
	if b.files["OUTLINE.md"] {
		b.outline = &page{kind: outlinePage, source: "OUTLINE.md", urlPath: "outline/"}
		b.addPage(b.outline)
	}
	var notes []string
	for name := range b.files {
		if path.Dir(name) == "docs/knowledge" && strings.HasSuffix(name, ".md") {
			notes = append(notes, name)
		}
	}
	sort.Strings(notes)
	for _, name := range notes {
		slug := strings.TrimSuffix(path.Base(name), ".md")
		p := &page{kind: knowledgePage, source: name, urlPath: "knowledge/" + slug + "/"}
		b.addPage(p)
		b.notes = append(b.notes, p)
	}
	return nil
}

func (b *builder) load(p *page) error {
	raw, err := os.ReadFile(filepath.Join(b.opts.Root, filepath.FromSlash(p.source)))
	if err != nil {
		return err
	}
	body := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	frontTitle := ""
	// Only knowledge notes carry YAML front matter; stripping "---" on README,
	// outline, or chapter pages would discard text after a leading thematic break.
	if p.kind == knowledgePage {
		body, frontTitle = stripFrontMatter(body)
	}
	p.doc = parseMarkdown(body)
	p.title = p.doc.title
	if p.title == "" {
		p.title = frontTitle
	}
	if p.title == "" {
		p.title = strings.TrimSuffix(path.Base(p.source), ".md")
	}
	if p.kind == chapterPage {
		p.chapter.rung = findRung(proseOnly(p.doc))
	}
	return nil
}

var frontTitlePattern = regexp.MustCompile(`(?m)^title:\s*(.*?)\s*$`)

// stripFrontMatter removes a leading YAML block delimited by "---" lines
// (including empty front matter or a closing "---" at EOF) and returns any
// title field from it.
func stripFrontMatter(normalized []byte) ([]byte, string) {
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return normalized, ""
	}
	offset := 4
	for offset <= len(normalized) {
		lineEnd := bytes.IndexByte(normalized[offset:], '\n')
		next := offset + lineEnd + 1
		if lineEnd < 0 {
			lineEnd = len(normalized) - offset
			next = len(normalized)
		}
		if strings.TrimRight(string(normalized[offset:offset+lineEnd]), " \t") == "---" {
			header := normalized[4:offset]
			title := ""
			if m := frontTitlePattern.FindSubmatch(header); m != nil {
				title = unquoteYAMLScalar(string(m[1]))
			}
			return normalized[next:], title
		}
		if lineEnd == len(normalized)-offset {
			break
		}
		offset = next
	}
	return normalized, ""
}

func unquoteYAMLScalar(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		return value[1 : len(value)-1]
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

var (
	rungDeclarationPattern = regexp.MustCompile(`\*\*The Rebuild Ladder \(rung (R\d+)\)\.?\*\*`)
	rungIDPattern          = regexp.MustCompile(`Rebuild Ladder \(rung (R\d+)\)`)
)

// findRung extracts a chapter's Rebuild Ladder rung from code-stripped prose.
// It prefers the bold rung declaration over an unbolded mention so a chapter
// referencing an earlier rung first still gets its own rung.
func findRung(body []byte) string {
	m := rungDeclarationPattern.FindSubmatch(body)
	if m == nil {
		m = rungIDPattern.FindSubmatch(body)
	}
	if m == nil {
		return ""
	}
	id := string(m[1])
	named := regexp.MustCompile(`\*\*` + id + ` \(([^)*]+)\)\*\*`).FindSubmatch(body)
	if named == nil {
		return id
	}
	return id + " (" + string(named[1]) + ")"
}

var experimentIDPattern = regexp.MustCompile(`^\d+\.\d+$`)

var experimentHeadingPattern = regexp.MustCompile(`^(?:[\d.]+\s+)?(Experiment (\d+\.\d+)\b.*)$`)

// findExperiments pairs level-3 experiment headings with their N.M-* directory
// and appends any numbered experiment directory that has no heading yet,
// ignoring non-numbered helper directories under experiments/.
func (b *builder) findExperiments(c *chapter) {
	seen := map[string]bool{}
	for _, h := range c.page.doc.headings {
		if h.level != 3 {
			continue
		}
		m := experimentHeadingPattern.FindStringSubmatch(h.text)
		if m == nil || seen[m[2]] {
			continue
		}
		seen[m[2]] = true
		c.experiments = append(c.experiments, experiment{id: m[2], dir: b.experimentDir(c, m[2]), heading: m[1], anchor: h.id})
	}
	var orphans []string
	for dir := range b.dirs {
		parent, name := path.Split(dir)
		if parent != c.dir+"/experiments/" {
			continue
		}
		id, _, ok := strings.Cut(name, "-")
		if ok && experimentIDPattern.MatchString(id) && !seen[id] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		id, _, _ := strings.Cut(name, "-")
		c.experiments = append(c.experiments, experiment{id: id, dir: "experiments/" + name, heading: name})
	}
	sort.SliceStable(c.experiments, func(i, j int) bool {
		return lessExperimentID(c.experiments[i].id, c.experiments[j].id)
	})
}

func lessExperimentID(a, b string) bool {
	var a1, a2, b1, b2 int
	fmt.Sscanf(a, "%d.%d", &a1, &a2)
	fmt.Sscanf(b, "%d.%d", &b1, &b2)
	if a1 != b1 {
		return a1 < b1
	}
	return a2 < b2
}

func (b *builder) experimentDir(c *chapter, id string) string {
	var match []string
	for dir := range b.dirs {
		parent, name := path.Split(dir)
		if parent == c.dir+"/experiments/" && strings.HasPrefix(name, id+"-") {
			match = append(match, name)
		}
	}
	sort.Strings(match)
	if len(match) == 0 {
		return ""
	}
	return "experiments/" + match[0]
}

func (b *builder) pageURL(p *page) string {
	return b.opts.Base + p.urlPath
}

func (b *builder) githubURL(repoPath string, isDir bool) string {
	kind := "blob"
	if isDir {
		kind = "tree"
	}
	segments := strings.Split(repoPath, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return b.opts.RepoURL + "/" + kind + "/" + b.opts.Branch + "/" + strings.Join(segments, "/")
}

var schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// resolve rewrites a markdown destination relative to source into a site page
// URL or a GitHub blob/tree URL. Query strings are ignored when locating a
// target and preserved only on links rewritten to GitHub.
func (b *builder) resolve(source, dest string) (string, string, bool) {
	if dest == "" || schemePattern.MatchString(dest) || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, "#") {
		return dest, "", true
	}
	pathPart, fragment, hasFragment := strings.Cut(dest, "#")
	pathPart, query, hasQuery := strings.Cut(pathPart, "?")
	suffix := ""
	if hasFragment {
		suffix = "#" + fragment
	}
	if pathPart == "" {
		return dest, "", true
	}
	pathPart = string(util.UnescapePunctuations([]byte(pathPart)))
	if strings.HasPrefix(pathPart, "/") {
		return dest, "absolute link " + dest, false
	}
	unescaped, err := url.PathUnescape(pathPart)
	if err != nil {
		return dest, "malformed link " + dest, false
	}
	target := path.Clean(path.Join(path.Dir(source), unescaped))
	if target == ".." || strings.HasPrefix(target, "../") {
		return dest, "link leaves the repository: " + dest, false
	}
	if target == "." {
		return b.githubURL("", true), "", true
	}
	if p, ok := b.bySource[target]; ok {
		if p == b.bySource[source] && hasFragment {
			return suffix, "", true
		}
		return b.pageURL(p) + suffix, "", true
	}
	if p, ok := b.byDir[target]; ok {
		return b.pageURL(p) + suffix, "", true
	}
	githubSuffix := suffix
	if hasQuery {
		githubSuffix = "?" + query + suffix
	}
	if b.files[target] {
		return b.githubURL(target, false) + githubSuffix, "", true
	}
	if b.dirs[target] {
		return b.githubURL(target, true) + githubSuffix, "", true
	}
	return dest, "link to a path not in the repository: " + dest, false
}

func (b *builder) rewrite(p *page, dest string) string {
	resolved, problem, ok := b.resolve(p.source, dest)
	if !ok {
		b.problems = append(b.problems, Problem{p.source, problem})
	}
	return resolved
}
