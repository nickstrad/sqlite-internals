package repocheck

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// SourceEntry represents one line of a chapter's SOURCES.sha256 manifest,
// pairing a SHA-256 hex digest with a path inside the pinned SQLite tree.
type SourceEntry struct {
	SHA256 string
	Path   string
	Line   int
}

var sourceLine = regexp.MustCompile(`^([0-9a-f]{64}) [ *]?(\S+)$`)

// ParseSources parses a SOURCES.sha256 manifest, flagging malformed lines and
// distinct tree paths that collide on the same base filename at the chapter root.
func ParseSources(text string) ([]SourceEntry, []Problem) {
	var entries []SourceEntry
	var problems []Problem
	seen := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := sourceLine.FindStringSubmatch(line)
		if m == nil {
			problems = append(problems, Problem{fmt.Sprintf("SOURCES.sha256:%d", i+1), fmt.Sprintf("not '<sha256>  <path>': %q", line)})
			continue
		}
		base := path.Base(m[2])
		if seen[base] {
			problems = append(problems, Problem{fmt.Sprintf("SOURCES.sha256:%d", i+1), "two entries copy to the same file name " + base})
		}
		seen[base] = true
		entries = append(entries, SourceEntry{SHA256: m[1], Path: m[2], Line: i + 1})
	}
	return entries, problems
}

func fileSHA256(name string) (string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

var chapterRootAllowed = map[string]bool{"SOURCES.sha256": true, "SPANS.txt": true, "answers.md": true}

// CheckSources verifies every entry in chapter/SOURCES.sha256 against both the
// pinned SQLite tree and the unmodified copy at the chapter root, and flags
// unlisted files or stray .go files at the chapter root.
func CheckSources(chapter Chapter, tree string) ([]Problem, int, error) {
	var problems []Problem
	sourcesFile := filepath.Join(chapter.Path, "SOURCES.sha256")
	listed := map[string]bool{}
	data, err := os.ReadFile(sourcesFile)
	if os.IsNotExist(err) {
		problems = append(problems, Problem{chapter.Dir + "/SOURCES.sha256", "missing SOURCES.sha256 (every chapter lists its copied SQLite files, even when there are none)"})
	} else if err != nil {
		return nil, 0, err
	}
	entries, parseProblems := ParseSources(string(data))
	for _, p := range parseProblems {
		p.Where = chapter.Dir + "/" + p.Where
		problems = append(problems, p)
	}
	for _, entry := range entries {
		where := fmt.Sprintf("%s/SOURCES.sha256:%d", chapter.Dir, entry.Line)
		base := path.Base(entry.Path)
		listed[base] = true
		pinned, err := fileSHA256(filepath.Join(tree, filepath.FromSlash(entry.Path)))
		if err != nil {
			problems = append(problems, Problem{where, fmt.Sprintf("%s is not in the pinned tree: %v", entry.Path, err)})
			continue
		}
		if pinned != entry.SHA256 {
			problems = append(problems, Problem{where, fmt.Sprintf("%s: listed sha256 %s, pinned tree has %s", entry.Path, entry.SHA256, pinned)})
		}
		copied, err := fileSHA256(filepath.Join(chapter.Path, base))
		if err != nil {
			problems = append(problems, Problem{where, fmt.Sprintf("copy %s missing from the chapter directory", base)})
			continue
		}
		if copied != pinned {
			problems = append(problems, Problem{where, fmt.Sprintf("copy %s differs from the pinned %s", base, entry.Path)})
		}
	}
	rootEntries, err := os.ReadDir(chapter.Path)
	if err != nil {
		return nil, 0, err
	}
	for _, entry := range rootEntries {
		name := entry.Name()
		if entry.IsDir() || listed[name] || chapterRootAllowed[name] || name == chapter.Dir+".md" {
			continue
		}
		// Any .go file beside copied SQLite .c files makes the Go toolchain treat the directory as a cgo package.
		if strings.HasSuffix(name, ".go") {
			problems = append(problems, Problem{chapter.Dir + "/" + name, "a chapter root holds no Go files (it would make the copied C a Go package)"})
			continue
		}
		problems = append(problems, Problem{chapter.Dir + "/" + name, "file at the chapter root is not listed in SOURCES.sha256"})
	}
	return problems, len(entries), nil
}

// SourcesLine formats one sha256sum-compatible line for SOURCES.sha256.
func SourcesLine(sha, treePath string) string {
	return sha + "  " + treePath
}

// ReadLines reads a text file into a slice of lines without trailing newlines.
func ReadLines(name string) ([]string, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}
