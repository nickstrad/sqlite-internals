package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SpanHeaderWindow is the number of lines starting at Span.Start within which
// Span.Symbol must appear. Allowing 12 lines covers C functions whose cited
// span starts on a leading comment block before the function signature.
const SpanHeaderWindow = 12

// Span records one cited source location from a chapter's SPANS.txt file:
// a path in the pinned SQLite tree, a 1-based inclusive line range, and the
// expected symbol name.
type Span struct {
	File   string
	Start  int
	End    int
	Symbol string
	Line   int
}

// ParseSpans parses lines of the form "file start end symbol", skipping blank
// lines and '#' comments.
func ParseSpans(text string) ([]Span, []Problem) {
	var spans []Span
	var problems []Problem
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		where := fmt.Sprintf("SPANS.txt:%d", i+1)
		if len(fields) != 4 {
			problems = append(problems, Problem{where, fmt.Sprintf("want 'file start end symbol', got %q", line)})
			continue
		}
		start, errStart := strconv.Atoi(fields[1])
		end, errEnd := strconv.Atoi(fields[2])
		if errStart != nil || errEnd != nil || start < 1 || end < start {
			problems = append(problems, Problem{where, fmt.Sprintf("bad line range %s-%s", fields[1], fields[2])})
			continue
		}
		spans = append(spans, Span{File: fields[0], Start: start, End: end, Symbol: fields[3], Line: i + 1})
	}
	return spans, problems
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// symbolMatcher requires word boundaries for plain C identifiers so a symbol
// like unixLock does not falsely match unixLockHelper; non-identifier tokens
// (such as "balance(") match as substrings.
func symbolMatcher(symbol string) func(string) bool {
	if identifierPattern.MatchString(symbol) {
		pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(symbol) + `($|[^A-Za-z0-9_])`)
		return pattern.MatchString
	}
	return func(line string) bool { return strings.Contains(line, symbol) }
}

// CheckSpan checks that span fits inside lines and that span.Symbol appears
// within lines[span.Start-1 : min(span.Start+SpanHeaderWindow-1, span.End)].
// It returns an empty string on success or a diagnostic naming the first other
// line where the symbol appears.
func CheckSpan(tree string, span Span, lines []string) string {
	if span.End > len(lines) {
		return fmt.Sprintf("%s has %d lines, span ends at %d", span.File, len(lines), span.End)
	}
	matches := symbolMatcher(span.Symbol)
	last := min(span.Start+SpanHeaderWindow-1, span.End)
	for n := span.Start; n <= last; n++ {
		if matches(lines[n-1]) {
			return ""
		}
	}
	for n, line := range lines {
		if matches(line) && n+1 != span.Start {
			return fmt.Sprintf("%s not found in lines %d-%d of %s (first mention at line %d)", span.Symbol, span.Start, last, span.File, n+1)
		}
	}
	return fmt.Sprintf("%s not found in lines %d-%d of %s (not in the file at all)", span.Symbol, span.Start, last, span.File)
}

// CheckSpans verifies every entry in chapter/SPANS.txt against the pinned tree.
// A chapter with no SPANS.txt returns zero problems and zero spans checked.
func CheckSpans(chapter Chapter, tree string) ([]Problem, int, error) {
	data, err := os.ReadFile(filepath.Join(chapter.Path, "SPANS.txt"))
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	spans, problems := ParseSpans(string(data))
	for i := range problems {
		problems[i].Where = chapter.Dir + "/" + problems[i].Where
	}
	files := map[string][]string{}
	for _, span := range spans {
		where := fmt.Sprintf("%s/SPANS.txt:%d", chapter.Dir, span.Line)
		lines, ok := files[span.File]
		if !ok {
			lines, err = ReadLines(filepath.Join(tree, filepath.FromSlash(span.File)))
			if err != nil {
				problems = append(problems, Problem{where, fmt.Sprintf("%s is not in the pinned tree", span.File)})
				continue
			}
			files[span.File] = lines
		}
		if message := CheckSpan(tree, span, lines); message != "" {
			problems = append(problems, Problem{where, message})
		}
	}
	return problems, len(spans), nil
}
