package repocheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepPathsReaderFacingRules(t *testing.T) {
	home := "/" + "home" + "/" + "alice" + "/"
	mac := "/" + "Users" + "/" + "bob" + "/"
	repo := newGitRepo(t)
	repo.commit("Opus", "files", map[string]string{
		"tools/bin/tool":        "ELF\x00\x01" + ForbiddenPathFragment + "baked in\n",
		"README.md":             "# Book\n\nSee [one](ch01-a/ch01-a.md#x), [dir](docs/), [plan](OUTLINE.md \"title\") and [gone](missing.md).\n",
		"OUTLINE.md":            "[ref]: ./docs/a.md\n[bad]: nowhere.md\n",
		"ch01-a/ch01-a.md":      "Up: [readme](../README.md). Out: [x](../../outside.md). Abs: [y](/etc/hosts).\n```go\n[z](inside-fence.md)\n```\nCode `[w](in-code.md)` ok.\n",
		"docs/a.md":             "cd " + home + "src and " + mac + "x and " + ForbiddenPathFragment + "y\nnot flagged: /home/<name>/ or https://example.org" + ForbiddenPathFragment + "z or a" + ForbiddenPathFragment + "b\n",
		"docs/links.md":         "[s](https://sqlite.org/fileformat.html) [g](https://github.com/nickstrad/sqlite-internals/blob/main/x) [p](https://nickstrad.github.io/sqlite-internals/) <https://proxy.golang.org/> https://github.com/sqlite/sqlite/blob/x.\nhttps://github.com/other/repo, https://github.com/other/repo and https://github.com/nickstrad/other-repo\n",
		"agent_work/notes.md":   ForbiddenPathFragment + "fine [broken](nope.md)\n",
		"kb/vendored.md":        mac + "fine\n",
		"STATE.md":              home + "fine\n",
		"internal/code.go":      "package internal\n\nvar dir = \"" + ForbiddenPathFragment + "x\"\n",
		"docs/plain.txt":        "[not markdown](missing.txt)\n",
		"internal/code_test.go": "package internal\n\nvar fixture = \"https://github.com/fixture/only\"\n",
	})
	repo.commit("Opus", "ignore tools", map[string]string{".gitignore": "/tools/\n/untracked.md\n"})
	if err := os.WriteFile(filepath.Join(repo.dir, "untracked.md"), []byte(ForbiddenPathFragment+"x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo.dir, "loose.md"), []byte(ForbiddenPathFragment+"x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sweep, err := SweepPaths(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range sweep.Problems {
		got = append(got, p.Where+" "+p.Message)
	}
	want := []string{
		"OUTLINE.md:2 relative link nowhere.md does not resolve in the repository (nowhere.md)",
		"README.md:3 relative link missing.md does not resolve in the repository (missing.md)",
		"ch01-a/ch01-a.md:1 relative link ../../outside.md does not resolve in the repository (../outside.md)",
		"ch01-a/ch01-a.md:1 relative link /etc/hosts does not resolve in the repository (/etc/hosts)",
		"docs/a.md:1 mentions a machine-local path " + home + ", " + mac + ", " + ForbiddenPathFragment + ": cd " + home + "src and " + mac + "x and " + ForbiddenPathFragment + "y",
		"internal/code.go:3 mentions a machine-local path " + ForbiddenPathFragment + ": var dir = \"" + ForbiddenPathFragment + "x\"",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(sweep.Binary) != 1 || sweep.Binary[0] != "tools/bin/tool" {
		t.Fatalf("binary = %v", sweep.Binary)
	}
	if len(sweep.IgnoredButTracked) != 1 || sweep.IgnoredButTracked[0] != "tools/bin/tool" {
		t.Fatalf("ignored but tracked = %v", sweep.IgnoredButTracked)
	}
	var external []string
	for _, link := range sweep.External {
		external = append(external, link.URL+" "+link.Where)
		if link.URL == "https://github.com/other/repo" && link.Count != 2 {
			t.Errorf("count = %d", link.Count)
		}
	}
	wantExternal := []string{
		"https://example.org" + strings.TrimSuffix(ForbiddenPathFragment, "/") + "/z docs/a.md:2",
		"https://github.com/nickstrad/other-repo docs/links.md:2",
		"https://github.com/other/repo docs/links.md:2",
	}
	if strings.Join(external, "\n") != strings.Join(wantExternal, "\n") {
		t.Fatalf("external:\n%s\nwant:\n%s", strings.Join(external, "\n"), strings.Join(wantExternal, "\n"))
	}
	if sweep.Checked != 9 {
		t.Fatalf("checked = %d", sweep.Checked)
	}
}

func TestIsPublicReference(t *testing.T) {
	cases := map[string]bool{
		"https://sqlite.org/wal.html":                                  true,
		"https://www.sqlite.org/wal.html":                              true,
		"https://github.com/sqlite/sqlite/blob/version-3.53.4/src/x.c": true,
		"https://github.com/sqlite/sqlite-other":                       false,
		"https://github.com/nickstrad/sqlite-internals":                true,
		"https://github.com/nickstrad/sqlite-internals-fork":           false,
		"https://nickstrad.github.io/sqlite-internals/ch01/":           true,
		"https://nickstrad.github.io/other/":                           false,
		"https://pkg.go.dev/modernc.org/sqlite":                        true,
		"https://proxy.golang.org/":                                    true,
		"https://evilsqlite.org/":                                      false,
		"http://seaweedfs:8333":                                        false,
		"https://gitlab.com/cznic/sqlite":                              true,
		"https://gitlab.com/other":                                     false,
	}
	for link, want := range cases {
		if got := IsPublicReference(link); got != want {
			t.Errorf("IsPublicReference(%q) = %v, want %v", link, got, want)
		}
	}
}

func TestMarkdownLinksAndFences(t *testing.T) {
	links := MarkdownLinks("a [x](one.md) `[y](two.md)` [z](<three four.md>) [w](f(1).md) [v](five.md \"t\")")
	if strings.Join(links, ",") != "one.md,f(1).md,five.md,three four.md" {
		t.Fatalf("links = %v", links)
	}
	var fence fenceState
	lines := []string{"text", "````md", "```", "still code", "````", "> ```", "> quoted", "> ```", "after"}
	var code []bool
	for _, line := range lines {
		code = append(code, fence.inCode(line))
	}
	want := []bool{false, true, true, true, true, true, true, true, false}
	for i := range want {
		if code[i] != want[i] {
			t.Fatalf("line %d %q: inCode = %v", i, lines[i], code[i])
		}
	}
}
