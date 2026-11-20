package sqlitex

import (
	"path/filepath"
	"strings"
)

func DisplayPath(path string) string {
	root, err := RepoRoot()
	if err != nil {
		return filepath.Base(path)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}
