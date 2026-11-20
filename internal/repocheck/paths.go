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
}

var machinePathPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9._~-])(` + ForbiddenPathFragment + `|/home/[A-Za-z0-9._-]+/|/Users/[A-Za-z0-9._-]+/)`)

var (
	urlPattern           = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `|\\]+`)
	inlineLinkPattern    = regexp.MustCompile(`\]\(\s*<?([^()\s<>]+(?:\([^()\s]*\)[^()\s<>]*)*)>?(?:\s+"[^"]*")?\s*\)`)
	angleLinkPattern     = regexp.MustCompile(`\]\(\s*<([^<>\n]+)>(?:\s+"[^"]*")?\s*\)`)
	referenceLinkPattern = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s+.*)?$`)
	inlineCodePattern    = regexp.MustCompile("(`+)[^`]*?(`+)")
	urlSchemePattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

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
	for _, ref := range PublicReferences {
		subdomain := ref.pathPrefix == "" && strings.HasSuffix(host, "."+ref.host)
		if host != ref.host && !subdomain {
			continue
		}
		if ref.pathPrefix == "" || u.Path == ref.pathPrefix || strings.HasPrefix(u.Path, strings.TrimSuffix(ref.pathPrefix, "/")+"/") {
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
	for _, m := range machinePathPattern.FindAllStringSubmatch(line, -1) {
		found = append(found, m[1])
	}
	return found
}

func MarkdownLinks(line string) []string {
	stripped := inlineCodePattern.ReplaceAllString(line, "")
	var links []string
	for _, m := range inlineLinkPattern.FindAllStringSubmatch(stripped, -1) {
		links = append(links, m[1])
	}
	for _, m := range angleLinkPattern.FindAllStringSubmatch(stripped, -1) {
		links = append(links, m[1])
	}
	if m := referenceLinkPattern.FindStringSubmatch(stripped); m != nil {
		links = append(links, m[1])
	}
	return links
}

type fenceState struct {
	marker string
}

func (f *fenceState) inCode(line string) bool {
	trimmed := strings.TrimLeft(line, " \t>")
	trimmed = strings.TrimLeft(trimmed, " ")
	if f.marker != "" {
		if strings.HasPrefix(trimmed, f.marker) && strings.Trim(trimmed, f.marker[:1]+" \t") == "" {
			f.marker = ""
		}
		return true
	}
	for _, fence := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, fence) {
			run := len(trimmed) - len(strings.TrimLeft(trimmed, fence[:1]))
			f.marker = strings.Repeat(fence[:1], run)
			return true
		}
	}
	return false
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

func SweepPaths(root string) (PathSweep, error) {
	var sweep PathSweep
	files, err := TrackedFiles(root)
	if err != nil {
		return sweep, err
	}
	fileSet := map[string]bool{}
	dirSet := map[string]bool{}
	for _, name := range files {
		fileSet[name] = true
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
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
	external := map[string]*ExternalLink{}
	for _, name := range files {
		if hasPathPrefix(name, PathSweepExclude) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return sweep, err
		}
		if isBinary(data) {
			sweep.Binary = append(sweep.Binary, name)
			continue
		}
		sweep.Checked++
		markdown := strings.HasSuffix(name, ".md")
		var fence fenceState
		for i, line := range strings.Split(string(data), "\n") {
			where := fmt.Sprintf("%s:%d", name, i+1)
			if found := MachinePaths(line); len(found) > 0 {
				sweep.Problems = append(sweep.Problems, Problem{where, "mentions a machine-local path " + strings.Join(found, ", ") + ": " + strings.TrimSpace(line)})
			}
			for _, raw := range urlPattern.FindAllString(line, -1) {
				link := trimURL(raw)
				if IsPublicReference(link) {
					continue
				}
				if entry, ok := external[link]; ok {
					entry.Count++
				} else {
					external[link] = &ExternalLink{URL: link, Where: where, Count: 1}
				}
			}
			if !markdown || fence.inCode(line) {
				continue
			}
			for _, link := range MarkdownLinks(line) {
				if target, ok := ResolveRelativeLink(name, link, fileSet, dirSet); !ok {
					sweep.Problems = append(sweep.Problems, Problem{where, fmt.Sprintf("relative link %s does not resolve in the repository (%s)", link, target)})
				}
			}
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
