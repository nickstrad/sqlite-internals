package repocheck

import (
	"fmt"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const ProseAuthorPrefix = "Gemini 4 Neon"

type BlameLine struct {
	Line     int
	Commit   string
	Author   string
	Boundary bool
	Text     string
}

func ParseBlamePorcelain(text string) ([]BlameLine, error) {
	authors := map[string]string{}
	boundaries := map[string]bool{}
	var lines []BlameLine
	var current *BlameLine
	for _, raw := range strings.Split(text, "\n") {
		if raw == "" {
			continue
		}
		if strings.HasPrefix(raw, "\t") {
			if current == nil {
				return nil, fmt.Errorf("blame content line without header: %q", raw)
			}
			current.Text = raw[1:]
			current.Author = authors[current.Commit]
			current.Boundary = boundaries[current.Commit]
			lines = append(lines, *current)
			current = nil
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) >= 3 && len(fields[0]) == 40 && isHexString(fields[0]) {
			final, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, fmt.Errorf("blame header %q: %w", raw, err)
			}
			current = &BlameLine{Line: final, Commit: fields[0]}
			continue
		}
		if current == nil {
			continue
		}
		key, value, _ := strings.Cut(raw, " ")
		switch key {
		case "author":
			authors[current.Commit] = value
		case "boundary":
			boundaries[current.Commit] = true
		}
	}
	return lines, nil
}

func isHexString(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

var proseMarkdownPattern = regexp.MustCompile(`^ch\d\d-[^/]+/(ch\d\d-[^/]+\.md|answers\.md)$`)

func IsChapterProse(path string) bool {
	return proseMarkdownPattern.MatchString(path)
}

func goCommentLines(filename string, src []byte) (map[int]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	lines := map[int]bool{}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if IsDirective(comment.Text) {
				continue
			}
			start := fset.Position(comment.Pos()).Line
			end := fset.Position(comment.End()).Line
			for line := start; line <= end; line++ {
				lines[line] = true
			}
		}
	}
	return lines, nil
}

func markdownProseLines(path string, src []byte) map[int]bool {
	exempt := map[int]bool{}
	for _, include := range FindIncludeBlocks(string(src)) {
		for line := include.FenceStart; line <= include.FenceEnd; line++ {
			exempt[line] = true
		}
	}
	lines := map[int]bool{}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.TrimSpace(line) == "" || exempt[i+1] {
			continue
		}
		lines[i+1] = true
	}
	return lines
}

type ProseOptions struct {
	Exclude      []string
	AuthorPrefix string
}

func ParseRange(spec string) (string, string, error) {
	base, head, found := strings.Cut(spec, "..")
	if !found || base == "" || head == "" || strings.HasPrefix(head, ".") {
		return "", "", fmt.Errorf("want <base-rev>..<head-rev>, got %q", spec)
	}
	return base, head, nil
}

type ProseCounts struct {
	InRange     int
	BeforeRange int
}

func CheckProse(root, base, head string, options ProseOptions) ([]Problem, ProseCounts, error) {
	prefix := options.AuthorPrefix
	if prefix == "" {
		prefix = ProseAuthorPrefix
	}
	exclude := append([]string{"kb"}, options.Exclude...)
	var problems []Problem
	var counts ProseCounts
	files, err := FilesAtRevision(root, head)
	if err != nil {
		return nil, counts, err
	}
	sort.Strings(files)
	for _, name := range files {
		if hasPathPrefix(name, exclude) {
			continue
		}
		isGo := strings.HasSuffix(name, ".go")
		if !isGo && !IsChapterProse(name) {
			continue
		}
		src, err := FileAtRevision(root, head, name)
		if err != nil {
			return nil, counts, err
		}
		var proseLines map[int]bool
		if isGo {
			proseLines, err = goCommentLines(name, src)
			if err != nil {
				problems = append(problems, Problem{name, "does not parse: " + err.Error()})
				continue
			}
		} else {
			proseLines = markdownProseLines(name, src)
		}
		if len(proseLines) == 0 {
			continue
		}
		blame, err := Git(root, "blame", "--porcelain", "-w", "-M", "-C", base+".."+head, "--", name)
		if err != nil {
			return nil, counts, err
		}
		blamed, err := ParseBlamePorcelain(blame)
		if err != nil {
			return nil, counts, err
		}
		for _, line := range blamed {
			if !proseLines[line.Line] {
				continue
			}
			if line.Boundary {
				counts.BeforeRange++
				continue
			}
			counts.InRange++
			if strings.HasPrefix(line.Author, prefix) {
				continue
			}
			problems = append(problems, Problem{
				fmt.Sprintf("%s:%d", name, line.Line),
				fmt.Sprintf("introduced by %s (author %q), not %s: %s", line.Commit[:12], line.Author, prefix, strings.TrimSpace(line.Text)),
			})
		}
	}
	return problems, counts, nil
}
