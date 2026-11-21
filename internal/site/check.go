package site

import (
	"html"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	idAttrPattern      = regexp.MustCompile(`\sid\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	linkAttrPattern    = regexp.MustCompile(`\s(?:href|src)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)
)

func attrValue(m [][]byte) string {
	if m[1] != nil {
		return html.UnescapeString(string(m[1]))
	}
	return html.UnescapeString(string(m[2]))
}

type CheckResult struct {
	Problems []Problem
	Pages    int
	Links    int
}

func CheckSite(dir, base string) (CheckResult, error) {
	base = NormalizeBase(base)
	ids := map[string]map[string]bool{}
	links := map[string][]string{}
	duplicates := map[string][]string{}
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".html") {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data = htmlCommentPattern.ReplaceAll(data, nil)
		set := map[string]bool{}
		for _, m := range idAttrPattern.FindAllSubmatch(data, -1) {
			id := attrValue(m)
			if set[id] {
				duplicates[rel] = append(duplicates[rel], id)
			}
			set[id] = true
		}
		ids[rel] = set
		for _, m := range linkAttrPattern.FindAllSubmatch(data, -1) {
			links[rel] = append(links[rel], attrValue(m))
		}
		return nil
	})
	if err != nil {
		return CheckResult{}, err
	}
	var pages []string
	for page := range ids {
		pages = append(pages, page)
	}
	sort.Strings(pages)
	result := CheckResult{Pages: len(pages)}
	for _, page := range pages {
		for _, id := range duplicates[page] {
			result.Problems = append(result.Problems, Problem{page, "duplicate id " + strconv.Quote(id)})
		}
		for _, link := range links[page] {
			result.Links++
			if message := checkLink(dir, base, page, link, ids); message != "" {
				result.Problems = append(result.Problems, Problem{page, message})
			}
		}
	}
	return result, nil
}

func checkLink(dir, base, page, link string, ids map[string]map[string]bool) string {
	if link == "" {
		return "empty link"
	}
	if schemePattern.MatchString(link) || strings.HasPrefix(link, "//") {
		return ""
	}
	pathPart, fragment, hasFragment := strings.Cut(link, "#")
	pathPart, _, _ = strings.Cut(pathPart, "?")
	var target string
	switch {
	case pathPart == "":
		target = page
	case strings.HasPrefix(pathPart, "/"):
		if !strings.HasPrefix(pathPart, base) && pathPart+"/" != base {
			return "link outside the site base " + base + ": " + link
		}
		target = strings.TrimPrefix(pathPart, strings.TrimSuffix(base, "/"))
		target = strings.TrimLeft(target, "/")
	default:
		target = path.Join(path.Dir(page), pathPart)
		if strings.HasSuffix(pathPart, "/") {
			target += "/"
		}
	}
	unescaped, err := url.PathUnescape(target)
	if err != nil {
		return "malformed link " + link
	}
	target = unescaped
	if target == "" || strings.HasSuffix(target, "/") {
		target += "index.html"
	}
	target = path.Clean(target)
	if strings.HasPrefix(target, "../") || target == ".." {
		return "link leaves the site: " + link
	}
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(target)))
	if err != nil {
		return "broken link " + link
	}
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(target), "index.html")); err != nil {
			return "broken link " + link
		}
		target = path.Join(target, "index.html")
	}
	if !hasFragment || fragment == "" {
		return ""
	}
	set, ok := ids[target]
	if !ok {
		if strings.HasSuffix(target, ".html") {
			return "missing anchor #" + fragment + " in " + link
		}
		return ""
	}
	if decoded, err := url.PathUnescape(fragment); err == nil {
		fragment = decoded
	}
	if !set[fragment] {
		return "missing anchor #" + fragment + " in " + link
	}
	return ""
}
