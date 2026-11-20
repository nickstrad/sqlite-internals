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

// ProseAuthorPrefix is the required git author name prefix for every prose line
// (chapter markdown and Go comments) added in Phase 2.
const ProseAuthorPrefix = "Gemini 4 Neon"

// BlameLine holds the commit hash, author, boundary flag, and content of one
// line parsed from "git blame --porcelain".
type BlameLine struct {
	Line     int
	Commit   string
	Author   string
	Boundary bool
	Text     string
}

// ParseBlamePorcelain parses the porcelain output of git blame, carrying each
// commit's author and boundary status across repeated lines from the same commit.
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

// IsChapterProse reports whether path is a chapter markdown file or answers.md
// subject to prose authorship verification.
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

// markdownProseLines excludes fenced include blocks so refreshing inlined Go
// code does not flag code lines as non-Neon prose.
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

// ProseOptions configures excluded path prefixes and the required author prefix
// (defaulting to ProseAuthorPrefix) for CheckProse.
type ProseOptions struct {
	Exclude      []string
	AuthorPrefix string
}

// ParseRange splits a two-dot git revision range "<base>..<head>", rejecting
// missing bounds or three-dot symmetric ranges.
func ParseRange(spec string) (string, string, error) {
	base, head, found := strings.Cut(spec, "..")
	if !found || base == "" || head == "" || strings.HasPrefix(head, ".") {
		return "", "", fmt.Errorf("want <base-rev>..<head-rev>, got %q", spec)
	}
	return base, head, nil
}

// ProseCounts tallies how many prose lines were last changed inside base..head
// versus before base (git blame boundary lines).
type ProseCounts struct {
	InRange     int
	BeforeRange int
}

// CheckProse blames every non-directive Go comment line and every non-blank
// chapter markdown line (outside include blocks) across base..head, reporting
// any line introduced in that range whose git author does not start with
// AuthorPrefix.
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
