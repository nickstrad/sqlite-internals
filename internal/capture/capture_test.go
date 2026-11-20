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
	os.WriteFile(path, []byte(strings.Join(append(head, entries...), "\n")+"\n"), 0o644)
}

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
			f, _ := os.OpenFile(filepath.Join(c, PagesFile), os.O_WRONLY, 0)
			f.WriteAt([]byte{0xff}, 4096+100)
			f.Close()
		}, "capture page 2: image sha256"},
		{"short pages file", func(c string) {
			os.Truncate(filepath.Join(c, PagesFile), 4096*3)
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

func TestRefusesDatabaseNotAtRest(t *testing.T) {
	dir := t.TempDir()
	path := buildRecipe(t, dir, 2)
	os.WriteFile(path+"-wal", []byte("frames"), 0o644)
	if _, err := Pages(path, filepath.Join(dir, "a")); !errors.Is(err, ErrNotAtRest) || !strings.Contains(err.Error(), "-wal holds 6 bytes") {
		t.Fatal(err)
	}
	os.WriteFile(path+"-wal", nil, 0o644)
	if err := CheckAtRest(path); err != nil {
		t.Fatal("empty -wal should be at rest:", err)
	}
	os.Remove(path + "-wal")
	os.WriteFile(path+"-journal", []byte{0xd9, 0xd5, 0x05, 0xf9}, 0o644)
	if _, err := Pages(path, filepath.Join(dir, "b")); !errors.Is(err, ErrNotAtRest) || !strings.Contains(err.Error(), "hot -journal") {
		t.Fatal(err)
	}
	os.WriteFile(path+"-journal", make([]byte, 512), 0o644)
	if err := CheckAtRest(path); err != nil {
		t.Fatal("zeroed journal header should be at rest:", err)
	}
	os.Remove(path + "-journal")
	if _, err := os.Stat(filepath.Join(dir, "a", ManifestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manifest written for a refused capture")
	}
	if _, err := Pages(path, filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "x.db")
	os.WriteFile(out+"-wal", []byte("stale"), 0o644)
	if _, err := Rebuild(filepath.Join(dir, "c"), out); err == nil || !strings.Contains(err.Error(), "-wal exists") {
		t.Fatal(err)
	}
}
