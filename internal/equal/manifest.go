package equal

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const ManifestFormat = "sqlite-internals page manifest v1"

type ManifestEntry struct {
	Page   int
	Role   string
	SHA256 string
}

func (e ManifestEntry) String() string {
	return fmt.Sprintf("%d %s %s", e.Page, e.Role, e.SHA256)
}

type Manifest struct {
	PageSize int
	Entries  []ManifestEntry
}

func (m Manifest) PageCount() int { return len(m.Entries) }

func (m Manifest) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\npage-size %d\npages %d\n", ManifestFormat, m.PageSize, len(m.Entries))
	for _, e := range m.Entries {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Manifest) SHA256() string { return SHA256Hex([]byte(m.Text())) }

var (
	roleName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func ParseManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	scanner := bufio.NewScanner(r)
	line := 0
	next := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		line++
		return scanner.Text(), true
	}
	if text, ok := next(); !ok || text != ManifestFormat {
		return m, fmt.Errorf("manifest line 1: want %q", ManifestFormat)
	}
	readNumber := func(key string) (int, error) {
		text, ok := next()
		value, found := strings.CutPrefix(text, key+" ")
		if !ok || !found {
			return 0, fmt.Errorf("manifest line %d: want %q", line, key+" N")
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("manifest line %d: bad %s %q", line, key, value)
		}
		return n, nil
	}
	var err error
	if m.PageSize, err = readNumber("page-size"); err != nil {
		return m, err
	}
	declared, err := readNumber("pages")
	if err != nil {
		return m, err
	}
	for {
		text, ok := next()
		if !ok {
			break
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			return m, fmt.Errorf("manifest line %d: want 'page role sha256', got %q", line, text)
		}
		page, err := strconv.Atoi(fields[0])
		if err != nil || page < 1 {
			return m, fmt.Errorf("manifest line %d: bad page number %q", line, fields[0])
		}
		if !roleName.MatchString(fields[1]) || !sha256Re.MatchString(fields[2]) {
			return m, fmt.Errorf("manifest line %d: bad role or sha256 in %q", line, text)
		}
		m.Entries = append(m.Entries, ManifestEntry{Page: page, Role: fields[1], SHA256: fields[2]})
	}
	if err := scanner.Err(); err != nil {
		return m, err
	}
	if declared != len(m.Entries) {
		return m, fmt.Errorf("manifest declares %d pages and lists %d", declared, len(m.Entries))
	}
	return m, nil
}

type SequenceError struct {
	Line    int
	Page    int
	Problem string
}

func (e *SequenceError) Error() string {
	return fmt.Sprintf("manifest entry %d: page %d: %s", e.Line, e.Page, e.Problem)
}

func (m Manifest) CheckSequence() error {
	seenAt := map[int]int{}
	for i, e := range m.Entries {
		if first, dup := seenAt[e.Page]; dup {
			return &SequenceError{Line: i + 1, Page: e.Page, Problem: fmt.Sprintf("listed twice (entries %d and %d)", first, i+1)}
		}
		seenAt[e.Page] = i + 1
	}
	for i, e := range m.Entries {
		want := i + 1
		if e.Page == want {
			continue
		}
		if _, present := seenAt[want]; !present {
			return &SequenceError{Line: want, Page: want, Problem: fmt.Sprintf("missing (entry %d lists page %d)", want, e.Page)}
		}
		return &SequenceError{Line: want, Page: e.Page, Problem: fmt.Sprintf("out of order (entry %d should be page %d)", want, want)}
	}
	return nil
}

type ManifestDiff struct {
	Equal              bool
	FirstDifferingPage int
	DifferingPages     int
	Reason             string
}

func (d ManifestDiff) String() string {
	if d.Equal {
		return "manifests identical"
	}
	if d.FirstDifferingPage == 0 {
		return "manifests differ: " + d.Reason
	}
	return fmt.Sprintf("manifests differ: first differing page %d (%s), %d page(s) differ", d.FirstDifferingPage, d.Reason, d.DifferingPages)
}

func CompareManifests(a, b Manifest) ManifestDiff {
	if a.PageSize != b.PageSize {
		return ManifestDiff{Reason: fmt.Sprintf("page size %d vs %d", a.PageSize, b.PageSize)}
	}
	d := ManifestDiff{}
	for i := range max(len(a.Entries), len(b.Entries)) {
		var ea, eb ManifestEntry
		var reason string
		switch {
		case i >= len(a.Entries):
			eb = b.Entries[i]
			reason = "only in the second manifest"
		case i >= len(b.Entries):
			ea = a.Entries[i]
			reason = "only in the first manifest"
		default:
			ea, eb = a.Entries[i], b.Entries[i]
			switch {
			case ea.Page != eb.Page:
				reason = fmt.Sprintf("entry %d lists page %d vs %d", i+1, ea.Page, eb.Page)
			case ea.Role != eb.Role:
				reason = fmt.Sprintf("role %s vs %s", ea.Role, eb.Role)
			case ea.SHA256 != eb.SHA256:
				reason = "page image differs"
			}
		}
		if reason == "" {
			continue
		}
		if d.FirstDifferingPage == 0 {
			d.FirstDifferingPage = i + 1
			d.Reason = reason
		}
		d.DifferingPages++
	}
	d.Equal = d.DifferingPages == 0
	return d
}
