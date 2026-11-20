package repocheck

import (
	"bytes"
	"go/format"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PathSweepExclude lists tracked paths exempted from CheckPaths because they
// hold internal working scripts, vendored tooling, or handoff notes.
var PathSweepExclude = []string{"agent_work", "kb", "STATE.md"}

// ForbiddenPathFragment is the machine-local home directory prefix banned from
// reader-facing files. It is built by concatenation so this source file does
// not match its own check.
const ForbiddenPathFragment = "/" + "root" + "/"

func nestedModuleDirs(files []string) []string {
	var dirs []string
	for _, name := range files {
		if path.Base(name) == "go.mod" && name != "go.mod" {
			dirs = append(dirs, path.Dir(name))
		}
	}
	return dirs
}

// CheckGofmt verifies that every tracked .go file in the root module (skipping
// kb/ and nested probe modules that own their own go.mod) matches go/format.Source.
func CheckGofmt(root string) ([]Problem, int, error) {
	files, err := TrackedFiles(root)
	if err != nil {
		return nil, 0, err
	}
	exclude := append([]string{"kb"}, nestedModuleDirs(files)...)
	var problems []Problem
	checked := 0
	for _, name := range files {
		if !strings.HasSuffix(name, ".go") || hasPathPrefix(name, exclude) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		checked++
		formatted, err := format.Source(src)
		if err != nil {
			problems = append(problems, Problem{name, "does not parse: " + err.Error()})
			continue
		}
		if !bytes.Equal(src, formatted) {
			problems = append(problems, Problem{name, "not gofmt-formatted"})
		}
	}
	return problems, checked, nil
}
