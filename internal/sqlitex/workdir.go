package sqlitex

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const ModulePath = "github.com/nickstrad/sqlite-internals"

const WorkDirEnv = "COURSE_WORK_DIR"

var ErrRepoRootNotFound = errors.New("repository root not found: run from inside the sqlite-internals checkout")

func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return RepoRootFrom(dir)
}

func RepoRootFrom(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if isCourseModuleRoot(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrRepoRootNotFound
		}
		dir = parent
	}
}

func isCourseModuleRoot(dir string) bool {
	file, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1] == ModulePath
		}
	}
	return false
}

var workNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func WorkBase() (string, error) {
	if base := os.Getenv(WorkDirEnv); base != "" {
		return filepath.Abs(base)
	}
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "_work"), nil
}

func WorkDir(name string) (string, error) {
	if !workNamePattern.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid work directory name %q", name)
	}
	base, err := WorkBase()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, name)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func MustWorkDir(name string) string {
	dir, err := WorkDir(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: work directory:", err)
		os.Exit(1)
	}
	return dir
}
