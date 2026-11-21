package repocheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSweepPathsReaderFacingRules verifies machine-path detection, relative link checks, binary/exclude handling, and external URL reporting.
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
		"docs/links.md":         "[s](https://sqlite.org/fileformat.html) [g](https://github.com/nickstrad/walfleet/blob/main/x) [p](https://nickstrad.github.io/walfleet/) <https://proxy.golang.org/> https://github.com/sqlite/sqlite/blob/x.\nhttps://github.com/other/repo, https://github.com/other/repo and https://github.com/nickstrad/other-repo\n",
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

// TestIsPublicReference verifies host, subdomain, and path-prefix matching for allowlisted URLs.
func TestIsPublicReference(t *testing.T) {
	cases := map[string]bool{
		"https://sqlite.org/wal.html":                                  true,
		"https://www.sqlite.org/wal.html":                              true,
		"https://github.com/sqlite/sqlite/blob/version-3.53.4/src/x.c": true,
		"https://github.com/sqlite/sqlite-other":                       false,
		"https://github.com/nickstrad/walfleet":                        true,
		"https://github.com/nickstrad/walfleet-fork":                   false,
		"https://nickstrad.github.io/walfleet/ch01/":                   true,
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

// TestMarkdownLinksAndFences verifies CommonMark link extraction across CRLF fences, code spans, indented code, titles, and reference definitions.
func TestMarkdownLinksAndFences(t *testing.T) {
	source := "a [x](one.md) `[y](two.md)` [z](<three four.md>) [w](f(1).md) [v](five.md \"t\")\n" +
		"```go\r\n[in](fence.md)\r\n```\r\n\r\n[after](crlf.md)\n\n" +
		"Use `` ` `` and [swallowed](swallowed.md) and `ok`.\n\n" +
		"``use `[not](inside-code.md)` here``\n\n" +
		"```inline``` and [after-triple](triple.md)\n\n" +
		"     ```\n[after-indent](indent.md)\n\n" +
		"[s](single.md 'title') [p](paren.md (title)) [m](\nmulti.md\n)\n\n" +
		"[ok]: <has space.md>\n"
	var got []string
	for _, link := range MarkdownLinks([]byte(source)) {
		got = append(got, fmt.Sprintf("%d:%s", link.Line, link.Destination))
	}
	want := "1:one.md,1:three four.md,1:f(1).md,1:five.md,6:crlf.md,8:swallowed.md,12:triple.md,15:indent.md,17:single.md,17:paren.md,17:multi.md,21:has space.md"
	if strings.Join(got, ",") != want {
		t.Fatalf("links = %s\nwant    %s", strings.Join(got, ","), want)
	}
}

// TestIsPublicReferenceRejectsDotDot verifies dot-dot path traversal cannot bypass path-restricted allowlist entries.
func TestIsPublicReferenceRejectsDotDot(t *testing.T) {
	for _, link := range []string{
		"https://github.com/nickstrad/walfleet/../../evil/malware",
		"https://github.com/sqlite/sqlite/../evil/repo",
		"https://nickstrad.github.io/walfleet/../other-site/",
		"https://gitlab.com/cznic/../evil/project",
	} {
		if IsPublicReference(link) {
			t.Errorf("IsPublicReference(%q) = true", link)
		}
	}
}

// TestMachinePathsWithoutTrailingSlash verifies bare home-directory paths and lowercase /users/<name>/ are caught without false positives on placeholders.
func TestMachinePathsWithoutTrailingSlash(t *testing.T) {
	root := strings.TrimSuffix(ForbiddenPathFragment, "/")
	line := "cd " + root + " or HOME=/" + "home/alice or DIR=/" + "Users/bob or /" + "users/carol/src/ but not " + root + "s, a" + root + " or /home/<name>/"
	got := strings.Join(MachinePaths(line), ",")
	want := root + ",/" + "home/alice,/" + "Users/bob,/" + "users/carol/"
	if got != want {
		t.Fatalf("MachinePaths = %s, want %s", got, want)
	}
}

// TestSweepPathsSkipsUpstreamCopiesAndDeletedFiles verifies SOURCES.sha256 copies are skipped as targets only and deleted tracked files fail link resolution.
func TestSweepPathsSkipsUpstreamCopiesAndDeletedFiles(t *testing.T) {
	home := "/" + "home" + "/" + "drh" + "/"
	repo := newGitRepo(t)
	repo.commit("Opus", "files", map[string]string{
		"ch04-x/SOURCES.sha256": strings.Repeat("a", 64) + "  src/pager.c\n",
		"ch04-x/pager.c":        "** " + home + "a.db-journal https://bugzilla.example/1\n",
		"ch04-x/mine.c":         "** " + home + "b\n",
		"ch04-x/ch04-x.md":      "[copy](pager.c) [gone](gone.md)\n",
		"gone.md":               "# Gone\n",
	})
	if err := os.Remove(filepath.Join(repo.dir, "gone.md")); err != nil {
		t.Fatal(err)
	}
	sweep, err := SweepPaths(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range sweep.Problems {
		got = append(got, p.String())
	}
	want := []string{
		"ch04-x/ch04-x.md:1: relative link gone.md does not resolve in the repository (ch04-x/gone.md)",
		"ch04-x/mine.c:1: mentions a machine-local path " + home + ": ** " + home + "b",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(sweep.UpstreamCopies, ",") != "ch04-x/pager.c" || len(sweep.External) != 0 {
		t.Fatalf("upstream = %v external = %v", sweep.UpstreamCopies, sweep.External)
	}
}
