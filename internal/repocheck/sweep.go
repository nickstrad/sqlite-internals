package repocheck

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var PathSweepExclude = []string{"agent_work", "kb", "STATE.md"}

const ForbiddenPathFragment = "/root/"

func nestedModuleDirs(files []string) []string {
	var dirs []string
	for _, name := range files {
		if path.Base(name) == "go.mod" && name != "go.mod" {
			dirs = append(dirs, path.Dir(name))
		}
	}
	return dirs
}

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

func CheckPaths(root string) ([]Problem, int, error) {
	files, err := TrackedFiles(root)
	if err != nil {
		return nil, 0, err
	}
	var problems []Problem
	checked := 0
	for _, name := range files {
		if hasPathPrefix(name, PathSweepExclude) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, ForbiddenPathFragment) {
				problems = append(problems, Problem{fmt.Sprintf("%s:%d", name, i+1), "mentions a machine-local path: " + strings.TrimSpace(line)})
			}
		}
	}
	return problems, checked, nil
}
