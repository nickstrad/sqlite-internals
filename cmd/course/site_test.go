package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSiteDirRefusesRepositoryThroughSymlink(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "real-parent", "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".nojekyll"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-to-parent")
	if err := os.Symlink(filepath.Join(base, "real-parent"), link); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{filepath.Join(link, "repo"), link, repo, filepath.Join(repo, "..")} {
		if _, err := prepareSiteDir(repo, out); err == nil || !strings.Contains(err.Error(), "contains the repository") {
			t.Errorf("prepareSiteDir(%s) err = %v", out, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("repository damaged: %v", err)
	}
	dir, err := prepareSiteDir(repo, filepath.Join(link, "repo", "_site"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSiteDir(repo, filepath.Join(base, "..sibling")); err != nil {
		t.Fatalf("sibling refused: %v", err)
	}
}
