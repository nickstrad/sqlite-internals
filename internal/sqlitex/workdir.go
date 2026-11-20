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

// ModulePath is the Go module path of the course repository, used to distinguish
// the repository root from nested probe modules under agent_work/.
const ModulePath = "github.com/nickstrad/sqlite-internals"

// WorkDirEnv names the environment variable that overrides the default _work/
// base directory for experiment scratch files (used inside the lab container).
const WorkDirEnv = "COURSE_WORK_DIR"

// ErrRepoRootNotFound is returned when walking parent directories does not find
// a go.mod declaring ModulePath.
var ErrRepoRootNotFound = errors.New("repository root not found: run from inside the sqlite-internals checkout")

// RepoRoot locates the course repository root from the current working directory.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return RepoRootFrom(dir)
}

// RepoRootFrom walks upward from dir until it finds a directory whose go.mod
// declares ModulePath.
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

// isCourseModuleRoot checks the module line rather than mere presence of go.mod
// so probe subdirectories with their own go.mod files are skipped.
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

// WorkBase returns the parent directory for experiment scratch directories:
// $COURSE_WORK_DIR if set, otherwise <repo>/_work.
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

// WorkDir removes and recreates <WorkBase>/<name> so each experiment run starts
// from an empty directory. Names containing path separators, "..", or a leading
// dot are rejected to prevent accidental deletion outside WorkBase.
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

// MustWorkDir calls WorkDir and exits 1 on error.
func MustWorkDir(name string) string {
	dir, err := WorkDir(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: work directory:", err)
		os.Exit(1)
	}
	return dir
}

func DisplayPath(path string) string {
	root, err := RepoRoot()
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
		return path
	}
	return filepath.ToSlash(relative)
}
