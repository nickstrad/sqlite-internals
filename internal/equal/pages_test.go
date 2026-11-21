package equal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// TestComparePagesIgnoresOnlyTheDeclaredHeaderBytes verifies E2 ignore boundaries, page-2 sensitivity, and argument validation.
func TestComparePagesIgnoresOnlyTheDeclaredHeaderBytes(t *testing.T) {
	a := image('a', 'b', 'c')
	b := append([]byte(nil), a...)
	for _, offset := range []int{24, 27, 40, 43, 92, 99} {
		b[offset] ^= 0xff
	}
	r, err := ComparePages(a, b, 512)
	if err != nil || !r.Equal || r.Level != E2Pages {
		t.Fatal(r, err)
	}
	if want := "E2 pages: equal ignoring header bytes 24-27, 40-43, 92-99 (bytes differ only at [24 27 40 43 92 99])"; r.String() != want {
		t.Fatal(r.String())
	}
	if e1 := CompareImages(a, b, 512); e1.Equal || e1.FirstDifferingPage != 1 {
		t.Fatal(e1)
	}
	for _, offset := range []int{18, 19, 23, 28, 39, 44, 91, 100, 512 + 24, 2*512 + 95} {
		c := append([]byte(nil), b...)
		c[offset] ^= 0x01
		r, err := ComparePages(a, c, 512)
		if err != nil || r.Equal || r.FirstDifferingPage != offset/512+1 || r.DifferingPages != 1 || r.IgnoredDiffering != nil {
			t.Fatalf("offset %d: %v %v", offset, r, err)
		}
	}
	same, err := ComparePages(a, a, 512)
	if err != nil || !same.Equal || same.IgnoredDiffering != nil || !strings.HasPrefix(same.String(), "E2 pages: equal (sha256 ") {
		t.Fatal(same, err)
	}
	short, err := ComparePages(a, a[:1024], 512)
	if err != nil || short.Equal || short.FirstDifferingPage != 3 {
		t.Fatal(short, err)
	}
	if _, err := ComparePages(a, b, 0); err == nil {
		t.Fatal("page size 0 accepted")
	}
	if _, err := CompareImagesIgnoring(a, b, 512, []ByteRange{{5, 5}}); err == nil {
		t.Fatal("empty range accepted")
	}
	if E2Pages.String() != "E2 pages" || E3Logical.String() != "E3 logical" {
		t.Fatal(E2Pages, E3Logical)
	}
}

// TestBackupCopyIsEqualAtE2NotE1 verifies that a sqlite3_backup copy differs at E1 and matches at E2.
func TestBackupCopyIsEqualAtE2NotE1(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	source := buildDB(t, dir, "source.db", "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB)", "INSERT INTO t VALUES(1, zeroblob(5000))", "INSERT INTO t VALUES(2, zeroblob(10))")
	copyPath := filepath.Join(dir, "copy.db")
	db, err := OpenExisting(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitex.Backup(ctx, conn, copyPath, -1, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	db.Close()
	e1, err := CompareFiles(source, copyPath, 4096)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := ComparePageFiles(source, copyPath, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if e1.Equal || !e2.Equal || len(e2.IgnoredDiffering) == 0 {
		t.Fatalf("e1 %s; e2 %s", e1, e2)
	}
	for _, offset := range e2.IgnoredDiffering {
		if !ignored(DeclaredHeaderIgnore, offset) {
			t.Fatal(offset)
		}
	}
}

// TestChangedPages verifies added, removed, and byte-changed pages while skipping role-only changes.
func TestChangedPages(t *testing.T) {
	before := Manifest{PageSize: 512, Entries: []ManifestEntry{{1, "table-leaf", strings.Repeat("a", 64)}, {2, "table-leaf", strings.Repeat("b", 64)}, {3, "overflow", strings.Repeat("c", 64)}}}
	after := Manifest{PageSize: 512, Entries: []ManifestEntry{{1, "table-leaf", strings.Repeat("a", 64)}, {2, "table-interior", strings.Repeat("d", 64)}, {3, "freelist-leaf", strings.Repeat("c", 64)}, {4, "table-leaf", strings.Repeat("e", 64)}}}
	changes := ChangedPages(before, after)
	if got := FormatPageChanges(changes); got != "[2(table-leaf->table-interior) 4(new table-leaf)]" {
		t.Fatal(got)
	}
	if !changes[1].Added() || changes[0].Added() || PageNumbers(changes)[1] != 4 {
		t.Fatal(changes)
	}
	back := ChangedPages(after, before)
	if !back[1].Removed() || back[1].String() != "4(gone, was table-leaf)" {
		t.Fatal(back)
	}
	if len(ChangedPages(before, before)) != 0 {
		t.Fatal("a manifest changed against itself")
	}
}
