package sqlitex

import (
	"path/filepath"
	"strings"
)

// DisplayPath formats path relative to the repository root with forward slashes
// so captured evidence never leaks machine-specific directories. Any path
// outside the repository (including via a symlink) collapses to its base name.
func DisplayPath(path string) string {
	root, err := RepoRoot()
	if err != nil {
		return filepath.Base(path)
	}
	// Resolve symlinks on both ends so macOS /var -> /private/var and escaping
	// symlinks inside the tree are classified accurately.
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
