package capture

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
)

func buildRecipe(t *testing.T, dir string, steps int) string {
	t.Helper()
	path := filepath.Join(dir, "r.db")
	if err := recipe.Default().Build(context.Background(), path, steps); err != nil {
		t.Fatal(err)
	}
	return path
}

func integrity(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestCaptureAndRebuildAreByteIdentical verifies rung R1 capture and rebuild produce an E1-equal file passing integrity_check with no leftover temp file.
func TestCaptureAndRebuildAreByteIdentical(t *testing.T) {
	dir := t.TempDir()
	path := buildRecipe(t, dir, 8)
	m, err := Pages(path, filepath.Join(dir, "cap"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rebuilt.db")
	rebuilt, err := Rebuild(filepath.Join(dir, "cap"), out)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Text() != m.Text() {
		t.Fatal("manifest changed")
	}
	r, err := equal.CompareFiles(path, out, m.PageSize)
	if err != nil || !r.Equal {
		t.Fatal(r, err)
	}
	if got := integrity(t, out); got != "ok" {
		t.Fatal(got)
	}
	if _, err := os.Stat(out + ".rebuild-tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file left")
	}
}

func rewriteManifest(t *testing.T, dir string, edit func([]string) []string) {
	t.Helper()
	path := filepath.Join(dir, ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	head, entries := lines[:3], edit(append([]string(nil), lines[3:]...))
	head[2] = "pages " + strconv.Itoa(len(entries))
	must(t, os.WriteFile(path, []byte(strings.Join(append(head, entries...), "\n")+"\n"), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestRebuildNamesTheBadPage verifies dropped, duplicated, swapped, corrupted, and truncated captures are refused with the page named and no output written.
func TestRebuildNamesTheBadPage(t *testing.T) {
	dir := t.TempDir()
	path := buildRecipe(t, dir, 4)
	cases := []struct {
		name string
		edit func(capDir string)
		want string
	}{
		{"drop page 5", func(c string) {
			rewriteManifest(t, c, func(e []string) []string { return append(e[:4], e[5:]...) })
		}, "page 5: missing"},
		{"duplicate page 3", func(c string) {
			rewriteManifest(t, c, func(e []string) []string { return append(e[:4], append([]string{e[2]}, e[4:]...)...) })
		}, "page 3: listed twice"},
		{"swap pages 6 and 7", func(c string) {
			rewriteManifest(t, c, func(e []string) []string { e[5], e[6] = e[6], e[5]; return e })
		}, "page 7: out of order"},
		{"corrupt image of page 2", func(c string) {
			f, err := os.OpenFile(filepath.Join(c, PagesFile), os.O_WRONLY, 0)
			must(t, err)
			_, err = f.WriteAt([]byte{0xff}, 4096+100)
			must(t, err)
			must(t, f.Close())
		}, "capture page 2: image sha256"},
		{"short pages file", func(c string) {
			must(t, os.Truncate(filepath.Join(c, PagesFile), 4096*3))
		}, "holds 12288 bytes"},
	}
	for _, c := range cases {
		capDir := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
		if _, err := Pages(path, capDir); err != nil {
			t.Fatal(err)
		}
		c.edit(capDir)
		out := filepath.Join(dir, "out-"+filepath.Base(capDir)+".db")
		_, err := Rebuild(capDir, out)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s: output written", c.name)
		}
	}
}

// TestRefusesDatabaseNotAtRest verifies non-empty -wal, hot -journal, and stale output sidecars are refused while empty -wal and zeroed -journal are accepted.
func TestRefusesDatabaseNotAtRest(t *testing.T) {
	dir := t.TempDir()
	path := buildRecipe(t, dir, 2)
	must(t, os.WriteFile(path+"-wal", []byte("frames"), 0o644))
	if _, err := Pages(path, filepath.Join(dir, "a")); !errors.Is(err, ErrNotAtRest) || !strings.Contains(err.Error(), "-wal holds 6 bytes") {
		t.Fatal(err)
	}
	must(t, os.WriteFile(path+"-wal", nil, 0o644))
	if err := CheckAtRest(path); err != nil {
		t.Fatal("empty -wal should be at rest:", err)
	}
	must(t, os.Remove(path+"-wal"))
	must(t, os.WriteFile(path+"-journal", []byte{0xd9, 0xd5, 0x05, 0xf9}, 0o644))
	if _, err := Pages(path, filepath.Join(dir, "b")); !errors.Is(err, ErrNotAtRest) || !strings.Contains(err.Error(), "hot -journal") {
		t.Fatal(err)
	}
	must(t, os.WriteFile(path+"-journal", make([]byte, 512), 0o644))
	if err := CheckAtRest(path); err != nil {
		t.Fatal("zeroed journal header should be at rest:", err)
	}
	must(t, os.Remove(path+"-journal"))
	if _, err := os.Stat(filepath.Join(dir, "a", ManifestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manifest written for a refused capture")
	}
	if _, err := Pages(path, filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "x.db")
	must(t, os.WriteFile(out+"-wal", []byte("stale"), 0o644))
	if _, err := Rebuild(filepath.Join(dir, "c"), out); err == nil || !strings.Contains(err.Error(), "-wal exists") {
		t.Fatal(err)
	}
}

// TestRebuildRefusesACaptureTheCensusContradicts verifies Rebuild rejects
// captures that pass hash checks alone but fail the post-rebuild census:
// coordinated truncation of the last page, unknown or wrong page roles, and a
// zero-page manifest.
func TestRebuildRefusesACaptureTheCensusContradicts(t *testing.T) {
	dir := t.TempDir()
	path := buildRecipe(t, dir, 4)
	original := filepath.Join(dir, "original")
	m, err := Pages(path, original)
	must(t, err)
	pages, err := os.ReadFile(filepath.Join(original, PagesFile))
	must(t, err)
	last := m.PageCount()
	cases := []struct {
		name string
		edit func(capDir string)
		want string
	}{
		{"last page dropped from manifest and pages", func(c string) {
			rewriteManifest(t, c, func(e []string) []string { return e[:len(e)-1] })
			must(t, os.WriteFile(filepath.Join(c, PagesFile), pages[:(last-1)*m.PageSize], 0o644))
		}, "not a database the page census accepts"},
		{"unknown role for page 1", func(c string) {
			rewriteManifest(t, c, func(e []string) []string {
				f := strings.Fields(e[0])
				e[0] = strings.Join([]string{f[0], "bogus-role", f[2]}, " ")
				return e
			})
		}, "capture page 1: the manifest disagrees with the rebuilt file's census: role bogus-role vs table-leaf"},
		{"wrong role for page 1", func(c string) {
			rewriteManifest(t, c, func(e []string) []string {
				f := strings.Fields(e[0])
				e[0] = strings.Join([]string{f[0], "overflow", f[2]}, " ")
				return e
			})
		}, "capture page 1: the manifest disagrees with the rebuilt file's census: role overflow vs"},
		{"no pages", func(c string) {
			must(t, os.WriteFile(filepath.Join(c, ManifestFile), []byte(equal.ManifestFormat+"\npage-size 4096\npages 0\n"), 0o644))
			must(t, os.WriteFile(filepath.Join(c, PagesFile), nil, 0o644))
		}, "lists no pages"},
	}
	for _, c := range cases {
		capDir := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
		_, err := Pages(path, capDir)
		must(t, err)
		c.edit(capDir)
		out := filepath.Join(dir, "out-"+filepath.Base(capDir)+".db")
		_, err = Rebuild(capDir, out)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		for _, p := range []string{out, out + ".rebuild-tmp"} {
			if _, statErr := os.Stat(p); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("%s: %s left behind", c.name, p)
			}
		}
	}
}

// TestFailedCaptureLeavesNoCapture verifies that when a writer modifies the
// database mid-capture, Pages removes any prior capture and temp files so the
// target directory ends empty and Rebuild refuses it.
func TestFailedCaptureLeavesNoCapture(t *testing.T) {
	dir := t.TempDir()
	path := buildRecipe(t, dir, 3)
	capDir := filepath.Join(dir, "cap")
	_, err := Pages(path, capDir)
	must(t, err)
	afterCaptureWritten = func(dbPath string) {
		must(t, os.WriteFile(dbPath+"-wal", []byte("a writer started"), 0o644))
	}
	t.Cleanup(func() { afterCaptureWritten = func(string) {} })
	_, err = Pages(path, capDir)
	if !errors.Is(err, ErrNotAtRest) || !strings.Contains(err.Error(), "changed during the capture") {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(capDir)
	must(t, err)
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed capture left %v", names)
	}
	if _, err := Rebuild(capDir, filepath.Join(dir, "out.db")); err == nil {
		t.Fatal("Rebuild accepted the directory of a failed capture")
	}
}
