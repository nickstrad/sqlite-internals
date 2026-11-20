package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/repocheck"
)

// TestInlineCheckFromAnotherDirectory verifies inlineOne reads the chapter file by full path even when run outside the repo root.
func TestInlineCheckFromAnotherDirectory(t *testing.T) {
	root := t.TempDir()
	chapterDir := filepath.Join(root, "ch99-tmp")
	if err := os.MkdirAll(filepath.Join(chapterDir, "experiments", "99.1-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	code := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(chapterDir, "experiments", "99.1-x", "main.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	markdown := "# x\n\n<!-- include: experiments/99.1-x/main.go -->\n```go\n" + code + "```\n"
	if err := os.WriteFile(filepath.Join(chapterDir, "ch99-tmp.md"), []byte(markdown), 0o644); err != nil {
		t.Fatal(err)
	}
	chapters, err := repocheck.Chapters(root, "")
	if err != nil || len(chapters) != 1 {
		t.Fatalf("chapters %v %v", chapters, err)
	}
	t.Chdir(t.TempDir())
	var r report
	inlineOne(&r, chapters[0].Markdown(), relative(root, chapters[0].Markdown()), true)
	if r.failed {
		t.Fatal("inline -check failed when run outside the repository root")
	}
}

// TestRunsMustBePositive checks that check evidence and check all reject non-positive -runs with exit code 2.
func TestRunsMustBePositive(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"evidence", "-runs", "0"}, {"evidence", "-runs", "-3"}, {"all", "-runs", "0"}} {
		var exit exitError
		if err := runCheck(context.Background(), root, args); !errors.As(err, &exit) || exit.code != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}
