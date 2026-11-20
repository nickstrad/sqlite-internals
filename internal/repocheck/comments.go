package repocheck

import (
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"regexp"
	"sort"
	"strings"
)

func IsDirective(comment string) bool {
	if strings.HasPrefix(comment, "// +build") {
		return true
	}
	if !strings.HasPrefix(comment, "//") || len(comment) < 3 || comment[2] == ' ' {
		return false
	}
	body := comment[2:]
	if strings.HasPrefix(body, "line ") || strings.HasPrefix(body, "export ") || strings.HasPrefix(body, "extern ") {
		return true
	}
	name, rest, found := strings.Cut(body, ":")
	if !found || rest == "" {
		return false
	}
	switch name {
	case "go":
		verb, _, _ := strings.Cut(rest, " ")
		return goDirectives[verb]
	case "nolint":
		return nolintList.MatchString(rest)
	}
	return false
}

var goDirectives = map[string]bool{
	"build": true, "embed": true, "generate": true, "debug": true, "fix": true,
	"noinline": true, "nosplit": true, "noescape": true, "norace": true, "nocheckptr": true,
	"linkname": true, "uintptrescapes": true, "wasmimport": true, "wasmexport": true,
}

var nolintList = regexp.MustCompile(`^[a-z0-9_-]+(?:,[a-z0-9_-]+)*$`)

// CodeTokens parses src to confirm valid Go syntax and scans it into a flat
// stream of non-comment tokens plus any compiler directives. Comparing token
// streams rather than go/printer output avoids false diffs from blank lines
// left behind when comments are added or removed.
func CodeTokens(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, filename, src, parser.ParseComments|parser.SkipObjectResolution); err != nil {
		return nil, err
	}
	file := fset.AddFile(filename, -1, len(src))
	var s scanner.Scanner
	var scanErr error
	s.Init(file, src, func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, scanner.ScanComments)
	var tokens []string
	for {
		_, tok, literal := s.Scan()
		if tok == token.EOF {
			break
		}
		switch tok {
		case token.COMMENT:
			if IsDirective(literal) {
				tokens = append(tokens, "directive "+strings.TrimRight(literal, " \t\r\n"))
			}
			continue
		case token.SEMICOLON:
			tokens = append(tokens, ";")
			continue
		}
		if literal != "" {
			tokens = append(tokens, tok.String()+" "+literal)
		} else {
			tokens = append(tokens, tok.String())
		}
	}
	return tokens, scanErr
}

func collapseSemicolons(tokens []string) []string {
	var out []string
	for _, tok := range tokens {
		if tok == ";" && len(out) > 0 && out[len(out)-1] == ";" {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// SameCode reports whether before and after have identical Go token streams
// once non-directive comments are stripped and consecutive semicolons collapsed.
// When they differ, it returns a description of the first mismatching token.
func SameCode(filename string, before, after []byte) (bool, string, error) {
	a, err := CodeTokens(filename, before)
	if err != nil {
		return false, "", fmt.Errorf("old revision: %w", err)
	}
	b, err := CodeTokens(filename, after)
	if err != nil {
		return false, "", fmt.Errorf("new revision: %w", err)
	}
	a, b = collapseSemicolons(a), collapseSemicolons(b)
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return false, fmt.Sprintf("token %d: %q became %q", i+1, a[i], b[i]), nil
		}
	}
	if len(a) != len(b) {
		return false, fmt.Sprintf("token count %d became %d", len(a), len(b)), nil
	}
	return true, "", nil
}

// CommentsOptions configures path prefixes excluded from CheckComments (kb/ is
// always excluded).
type CommentsOptions struct {
	Exclude []string
}

func goFilesAt(root, revision string, exclude []string) (map[string]bool, error) {
	files, err := FilesAtRevision(root, revision)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, ".go") && !hasPathPrefix(name, exclude) {
			set[name] = true
		}
	}
	return set, nil
}

// CheckComments verifies that between oldRev and newRev no non-excluded .go
// file was added, removed, or modified outside its non-directive comments.
func CheckComments(root, oldRev, newRev string, options CommentsOptions) ([]Problem, int, error) {
	exclude := append([]string{"kb"}, options.Exclude...)
	oldFiles, err := goFilesAt(root, oldRev, exclude)
	if err != nil {
		return nil, 0, err
	}
	newFiles, err := goFilesAt(root, newRev, exclude)
	if err != nil {
		return nil, 0, err
	}
	all := map[string]bool{}
	for name := range oldFiles {
		all[name] = true
	}
	for name := range newFiles {
		all[name] = true
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []Problem
	for _, name := range names {
		switch {
		case !oldFiles[name]:
			problems = append(problems, Problem{name, "added between the two revisions"})
			continue
		case !newFiles[name]:
			problems = append(problems, Problem{name, "removed between the two revisions"})
			continue
		}
		before, err := FileAtRevision(root, oldRev, name)
		if err != nil {
			return nil, 0, err
		}
		after, err := FileAtRevision(root, newRev, name)
		if err != nil {
			return nil, 0, err
		}
		same, detail, err := SameCode(name, before, after)
		if err != nil {
			problems = append(problems, Problem{name, err.Error()})
			continue
		}
		if !same {
			problems = append(problems, Problem{name, "code changed, not only comments: " + detail})
		}
	}
	return problems, len(names), nil
}
