package equal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func image(pages ...byte) []byte {
	var out []byte
	for _, p := range pages {
		out = append(out, []byte(strings.Repeat(string(rune(p)), 512))...)
	}
	return out
}

// TestCompareImages verifies E1Bytes comparison for identical, differing, and differently sized images.
func TestCompareImages(t *testing.T) {
	a := image('a', 'b', 'c', 'd')
	if r := CompareImages(a, image('a', 'b', 'c', 'd'), 512); !r.Equal || r.Level != E1Bytes || r.FirstDifferingPage != 0 {
		t.Fatal(r)
	}
	r := CompareImages(a, image('a', 'x', 'c', 'y'), 512)
	if r.Equal || r.FirstDifferingPage != 2 || r.DifferingPages != 2 {
		t.Fatal(r)
	}
	r = CompareImages(a, image('a', 'b', 'c'), 512)
	if r.Equal || r.FirstDifferingPage != 4 || r.DifferingPages != 1 || !strings.Contains(r.String(), "sizes 2048 and 1536") {
		t.Fatal(r.String())
	}
	if E1Bytes.String() != "E1 bytes" {
		t.Fatal(E1Bytes.String())
	}
}

// TestCompareFilesAndHash checks CompareFiles, FileSHA256, and error reporting on missing files.
func TestCompareFilesAndHash(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, path := range []string{a, b} {
		if err := os.WriteFile(path, image('a', 'b'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := CompareFiles(a, b, 512)
	if err != nil || !r.Equal {
		t.Fatal(r, err)
	}
	sum, err := FileSHA256(a)
	if err != nil || sum != SHA256Hex(image('a', 'b')) || sum != r.SHA256A {
		t.Fatal(sum, err)
	}
	if _, err := CompareFiles(a, filepath.Join(dir, "missing"), 512); err == nil {
		t.Fatal("missing file accepted")
	}
}

func manifest(pages ...int) Manifest {
	m := Manifest{PageSize: 512}
	for _, p := range pages {
		m.Entries = append(m.Entries, ManifestEntry{Page: p, Role: "table-leaf", SHA256: SHA256Hex([]byte{byte(p)})})
	}
	return m
}

// TestManifestRoundTrip verifies manifest serialization, parsing, and rejection of malformed manifest inputs.
func TestManifestRoundTrip(t *testing.T) {
	m := manifest(1, 2, 3)
	parsed, err := ParseManifest(strings.NewReader(m.Text()))
	if err != nil || parsed.Text() != m.Text() || parsed.SHA256() != m.SHA256() {
		t.Fatal(err, parsed.Text())
	}
	for _, bad := range []string{
		"",
		"wrong\n",
		ManifestFormat + "\npage-size x\npages 0\n",
		ManifestFormat + "\npage-size 512\npages 2\n" + m.Entries[0].String() + "\n",
		ManifestFormat + "\npage-size 512\npages 1\n1 Table " + m.Entries[0].SHA256 + "\n",
		ManifestFormat + "\npage-size 512\npages 1\n1 table-leaf abc\n",
		ManifestFormat + "\npage-size 512\npages 1\n0 table-leaf " + m.Entries[0].SHA256 + "\n",
	} {
		if _, err := ParseManifest(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestCheckSequenceNamesThePage verifies missing, duplicate, and out-of-order entries report the offending page number.
func TestCheckSequenceNamesThePage(t *testing.T) {
	cases := []struct {
		pages []int
		page  int
		text  string
	}{
		{[]int{1, 2, 4, 5}, 3, "missing"},
		{[]int{1, 2, 2, 3}, 2, "listed twice"},
		{[]int{1, 3, 2, 4}, 3, "out of order"},
		{[]int{2, 3, 4}, 1, "missing"},
	}
	for _, c := range cases {
		err := manifest(c.pages...).CheckSequence()
		var seq *SequenceError
		if !errors.As(err, &seq) || seq.Page != c.page || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%v: %v", c.pages, err)
		}
	}
	if err := manifest(1, 2, 3).CheckSequence(); err != nil {
		t.Fatal(err)
	}
}

// TestCompareManifests checks entry-by-entry manifest diffing for image, role, length, and page-size differences.
func TestCompareManifests(t *testing.T) {
	a := manifest(1, 2, 3, 4)
	if d := CompareManifests(a, manifest(1, 2, 3, 4)); !d.Equal {
		t.Fatal(d)
	}
	b := manifest(1, 2, 3, 4)
	b.Entries[2].SHA256 = SHA256Hex([]byte("other"))
	b.Entries[3].Role = "overflow"
	d := CompareManifests(a, b)
	if d.Equal || d.FirstDifferingPage != 3 || d.DifferingPages != 2 || d.Reason != "page image differs" {
		t.Fatal(d)
	}
	d = CompareManifests(a, manifest(1, 2, 3))
	if d.FirstDifferingPage != 4 || !strings.Contains(d.String(), "only in the first") {
		t.Fatal(d)
	}
	c := manifest(1, 2)
	c.PageSize = 1024
	if d := CompareManifests(a, c); d.Equal || d.FirstDifferingPage != 0 {
		t.Fatal(d)
	}
}
