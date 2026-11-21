package repocheck

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type ExternalLink struct {
	URL   string
	Where string
	Count int
}

type PathSweep struct {
	Problems          []Problem
	Checked           int
	Binary            []string
	External          []ExternalLink
	IgnoredButTracked []string
	UpstreamCopies    []string
}

type MarkdownLink struct {
	Destination string
	Line        int
}

var machinePathPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9._~-])(` + strings.TrimSuffix(ForbiddenPathFragment, "/") + `|/home/[A-Za-z0-9._-]+|/[Uu]sers/[A-Za-z0-9._-]+)(/?)`)

var (
	urlPattern       = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `|\\]+`)
	urlSchemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

var linkParser = goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()

type publicReference struct {
	host       string
	pathPrefix string
}

var PublicReferences = []publicReference{
	{"github.com", "/nickstrad/sqlite-internals"},
	{"nickstrad.github.io", "/sqlite-internals"},
	{"sqlite.org", ""},
	{"github.com", "/sqlite/sqlite"},
	{"pkg.go.dev", ""},
	{"go.dev", ""},
	{"golang.org", ""},
	{"modernc.org", ""},
	{"gitlab.com", "/cznic/"},
}

func IsPublicReference(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	cleanPath := u.Path
	if cleanPath != "" {
		cleanPath = path.Clean(cleanPath)
	}
	for _, ref := range PublicReferences {
		subdomain := ref.pathPrefix == "" && strings.HasSuffix(host, "."+ref.host)
		if host != ref.host && !subdomain {
			continue
		}
		prefix := strings.TrimSuffix(ref.pathPrefix, "/")
		if prefix == "" || cleanPath == prefix || strings.HasPrefix(cleanPath, prefix+"/") {
			return true
		}
	}
	return false
}

func isBinary(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func trimURL(raw string) string {
	return strings.TrimRight(raw, ".,;:!?*_")
}

func MachinePaths(line string) []string {
	var found []string
	for _, loc := range machinePathPattern.FindAllStringSubmatchIndex(line, -1) {
		name, slash := line[loc[2]:loc[3]], line[loc[4]:loc[5]]
		if slash == "" {
			if loc[5] < len(line) && isPathNameByte(line[loc[5]]) {
				continue
			}
			name = strings.TrimRight(name, ".")
		}
		found = append(found, name+slash)
	}
	return found
}

func isPathNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func lineAt(starts []int, offset int) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
}

func MarkdownLinks(source []byte) []MarkdownLink {
	var starts []int
	starts = append(starts, 0)
	for i, c := range source {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	ctx := parser.NewContext()
	root := linkParser.Parse(text.NewReader(source), parser.WithContext(ctx))
	var links []MarkdownLink
	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.Link:
			links = append(links, MarkdownLink{string(t.Destination), lineAt(starts, t.Pos())})
		case *ast.Image:
			links = append(links, MarkdownLink{string(t.Destination), lineAt(starts, t.Pos())})
		}
		return ast.WalkContinue, nil
	})
	for _, ref := range ctx.References() {
		definition := regexp.MustCompile(`(?im)^ {0,3}\[` + regexp.QuoteMeta(string(ref.Label())) + `\]:`)
		line := 1
		if loc := definition.FindIndex(source); loc != nil {
			line = lineAt(starts, loc[0])
		}
		links = append(links, MarkdownLink{string(ref.Destination()), line})
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].Line < links[j].Line })
	return links
}

func ResolveRelativeLink(from, link string, files, dirs map[string]bool) (string, bool) {
	if link == "" || strings.HasPrefix(link, "#") || urlSchemePattern.MatchString(link) || strings.HasPrefix(link, "//") {
		return "", true
	}
	pathPart, _, _ := strings.Cut(link, "#")
	pathPart, _, _ = strings.Cut(pathPart, "?")
	if pathPart == "" {
		return "", true
	}
	pathPart = string(util.UnescapePunctuations([]byte(pathPart)))
	if strings.HasPrefix(pathPart, "/") {
		return pathPart, false
	}
	if unescaped, err := url.PathUnescape(pathPart); err == nil {
		pathPart = unescaped
	}
	target := path.Clean(path.Join(path.Dir(from), pathPart))
	if target == ".." || strings.HasPrefix(target, "../") {
		return target, false
	}
	return target, target == "." || files[target] || dirs[target]
}

func upstreamCopies(root string, files []string) map[string]bool {
	copies := map[string]bool{}
	for _, name := range files {
		if path.Base(name) != "SOURCES.sha256" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		entries, _ := ParseSources(string(data))
		for _, entry := range entries {
			copies[path.Join(path.Dir(name), path.Base(entry.Path))] = true
		}
	}
	return copies
}

type numberedProblem struct {
	line    int
	problem Problem
}

func SweepPaths(root string) (PathSweep, error) {
	var sweep PathSweep
	tracked, err := TrackedFiles(root)
	if err != nil {
		return sweep, err
	}
	var files []string
	fileSet := map[string]bool{}
	dirSet := map[string]bool{}
	for _, name := range tracked {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			continue
		}
		files = append(files, name)
		fileSet[name] = true
		for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
			dirSet[dir] = true
		}
	}
	if out, err := Git(root, "ls-files", "-z", "--cached", "--ignored", "--exclude-standard"); err == nil {
		for _, name := range strings.Split(out, "\x00") {
			if name != "" {
				sweep.IgnoredButTracked = append(sweep.IgnoredButTracked, name)
			}
		}
	}
	copies := upstreamCopies(root, files)
	external := map[string]*ExternalLink{}
	for _, name := range files {
		if hasPathPrefix(name, PathSweepExclude) {
			continue
		}
		if copies[name] {
			sweep.UpstreamCopies = append(sweep.UpstreamCopies, name)
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return sweep, err
		}
		if isBinary(data) {
			sweep.Binary = append(sweep.Binary, name)
			continue
		}
		sweep.Checked++
		reportExternal := !strings.HasSuffix(name, "_test.go")
		var found []numberedProblem
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			where := fmt.Sprintf("%s:%d", name, i+1)
			if paths := MachinePaths(line); len(paths) > 0 {
				found = append(found, numberedProblem{i + 1, Problem{where, "mentions a machine-local path " + strings.Join(paths, ", ") + ": " + strings.TrimSpace(line)}})
			}
			for _, raw := range urlPattern.FindAllString(line, -1) {
				link := trimURL(raw)
				if !reportExternal || IsPublicReference(link) {
					continue
				}
				if entry, ok := external[link]; ok {
					entry.Count++
				} else {
					external[link] = &ExternalLink{URL: link, Where: where, Count: 1}
				}
			}
		}
		if strings.HasSuffix(name, ".md") {
			reported := map[string]bool{}
			for _, link := range MarkdownLinks(data) {
				target, ok := ResolveRelativeLink(name, link.Destination, fileSet, dirSet)
				if ok || reported[link.Destination] {
					continue
				}
				reported[link.Destination] = true
				where := fmt.Sprintf("%s:%d", name, link.Line)
				found = append(found, numberedProblem{link.Line, Problem{where, fmt.Sprintf("relative link %s does not resolve in the repository (%s)", link.Destination, target)}})
			}
		}
		sort.SliceStable(found, func(i, j int) bool { return found[i].line < found[j].line })
		for _, p := range found {
			sweep.Problems = append(sweep.Problems, p.problem)
		}
	}
	for _, entry := range external {
		sweep.External = append(sweep.External, *entry)
	}
	sort.Slice(sweep.External, func(i, j int) bool { return sweep.External[i].URL < sweep.External[j].URL })
	return sweep, nil
}

func CheckPaths(root string) ([]Problem, int, error) {
	sweep, err := SweepPaths(root)
	return sweep.Problems, sweep.Checked, err
}
