package site

import (
	"bytes"
	"html"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type tagAttribute struct {
	name  string
	value string
}

var rawTextElements = map[string]bool{"script": true, "style": true, "textarea": true, "title": true}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func openingTagAttributes(data []byte) []tagAttribute {
	var attributes []tagAttribute
	n := len(data)
	i := 0
	for i < n {
		lt := bytes.IndexByte(data[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		if bytes.HasPrefix(data[i:], []byte("<!--")) {
			end := bytes.Index(data[i+4:], []byte("-->"))
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		if i+1 < n && (data[i+1] == '/' || data[i+1] == '!' || data[i+1] == '?') {
			end := bytes.IndexByte(data[i+1:], '>')
			if end < 0 {
				break
			}
			i += 1 + end + 1
			continue
		}
		if i+1 >= n || !isASCIILetter(data[i+1]) {
			i++
			continue
		}
		j := i + 1
		for j < n && !isHTMLSpace(data[j]) && data[j] != '/' && data[j] != '>' {
			j++
		}
		tagName := strings.ToLower(string(data[i+1 : j]))
		var tagAttributes []tagAttribute
		closed := false
		for j < n {
			for j < n && (isHTMLSpace(data[j]) || data[j] == '/') {
				j++
			}
			if j >= n {
				break
			}
			if data[j] == '>' {
				j++
				closed = true
				break
			}
			nameStart := j
			j++
			for j < n && !isHTMLSpace(data[j]) && data[j] != '/' && data[j] != '>' && data[j] != '=' {
				j++
			}
			attribute := tagAttribute{name: strings.ToLower(string(data[nameStart:j]))}
			k := j
			for k < n && isHTMLSpace(data[k]) {
				k++
			}
			if k < n && data[k] == '=' {
				k++
				for k < n && isHTMLSpace(data[k]) {
					k++
				}
				if k < n && (data[k] == '"' || data[k] == '\'') {
					quote := data[k]
					end := bytes.IndexByte(data[k+1:], quote)
					if end < 0 {
						j = n
						break
					}
					attribute.value = html.UnescapeString(string(data[k+1 : k+1+end]))
					j = k + 1 + end + 1
				} else {
					valueStart := k
					for k < n && !isHTMLSpace(data[k]) && data[k] != '>' {
						k++
					}
					attribute.value = html.UnescapeString(string(data[valueStart:k]))
					j = k
				}
			}
			tagAttributes = append(tagAttributes, attribute)
		}
		if !closed {
			break
		}
		attributes = append(attributes, tagAttributes...)
		i = j
		if rawTextElements[tagName] {
			closing := bytes.Index(bytes.ToLower(data[i:]), []byte("</"+tagName))
			if closing < 0 {
				break
			}
			i += closing
		}
	}
	return attributes
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
		set := map[string]bool{}
		for _, attribute := range openingTagAttributes(data) {
			switch attribute.name {
			case "id":
				if set[attribute.value] {
					duplicates[rel] = append(duplicates[rel], attribute.value)
				}
				set[attribute.value] = true
			case "href", "src":
				links[rel] = append(links[rel], attribute.value)
			}
		}
		ids[rel] = set
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
