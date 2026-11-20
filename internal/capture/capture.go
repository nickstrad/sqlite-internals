package capture

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickstrad/sqlite-internals/internal/dbfile"
	"github.com/nickstrad/sqlite-internals/internal/equal"
)

const (
	ManifestFile = "manifest.txt"
	PagesFile    = "pages.bin"
)

var ErrNotAtRest = errors.New("database is not at rest")

type NotAtRestError struct {
	Path   string
	Reason string
}

func (e *NotAtRestError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrNotAtRest, e.Path, e.Reason)
}

func (e *NotAtRestError) Unwrap() error { return ErrNotAtRest }

func CheckAtRest(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	wal, err := os.Stat(dbPath + "-wal")
	switch {
	case err == nil && wal.Size() > 0:
		return &NotAtRestError{Path: dbPath, Reason: fmt.Sprintf("-wal holds %d bytes of frames not yet checkpointed into the database file", wal.Size())}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	hot, size, err := hotJournal(dbPath + "-journal")
	if err != nil {
		return err
	}
	if hot {
		return &NotAtRestError{Path: dbPath, Reason: fmt.Sprintf("hot -journal (%d bytes with a live header): a transaction was interrupted and the next open will roll it back", size)}
	}
	return nil
}

func hotJournal(path string) (bool, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, 0, err
	}
	if info.Size() == 0 {
		return false, 0, nil
	}
	first := make([]byte, 1)
	if _, err := f.ReadAt(first, 0); err != nil {
		return false, info.Size(), err
	}
	return first[0] != 0, info.Size(), nil
}

func BuildManifest(f *dbfile.File) (equal.Manifest, error) {
	census, err := f.Census()
	if err != nil {
		return equal.Manifest{}, err
	}
	if err := census.Complete(); err != nil {
		return equal.Manifest{}, err
	}
	m := equal.Manifest{PageSize: f.PageSize(), Entries: make([]equal.ManifestEntry, 0, f.PageCount())}
	for n := 1; n <= f.PageCount(); n++ {
		page, err := f.Page(n)
		if err != nil {
			return equal.Manifest{}, err
		}
		m.Entries = append(m.Entries, equal.ManifestEntry{Page: n, Role: census.Page(n).Role.String(), SHA256: equal.SHA256Hex(page)})
	}
	return m, nil
}

func Pages(dbPath, dir string) (equal.Manifest, error) {
	if err := CheckAtRest(dbPath); err != nil {
		return equal.Manifest{}, err
	}
	before, err := os.Stat(dbPath)
	if err != nil {
		return equal.Manifest{}, err
	}
	f, err := dbfile.Open(dbPath)
	if err != nil {
		return equal.Manifest{}, err
	}
	defer f.Close()
	if int64(f.PageCount())*int64(f.PageSize()) != f.FileSize() {
		return equal.Manifest{}, fmt.Errorf("%s: file holds %d bytes but the database is %d pages of %d bytes; a page capture would not rebuild the file", dbPath, f.FileSize(), f.PageCount(), f.PageSize())
	}
	m, err := BuildManifest(f)
	if err != nil {
		return equal.Manifest{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return equal.Manifest{}, err
	}
	if err := writePages(f, filepath.Join(dir, PagesFile), m); err != nil {
		return equal.Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(m.Text()), 0o644); err != nil {
		return equal.Manifest{}, err
	}
	if err := CheckAtRest(dbPath); err != nil {
		return equal.Manifest{}, fmt.Errorf("database changed during the capture: %w", err)
	}
	after, err := os.Stat(dbPath)
	if err != nil {
		return equal.Manifest{}, err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return equal.Manifest{}, fmt.Errorf("%s: database changed during the capture", dbPath)
	}
	return m, nil
}

func writePages(f *dbfile.File, path string, m equal.Manifest) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	for _, e := range m.Entries {
		page, err := f.Page(e.Page)
		if err != nil {
			out.Close()
			return err
		}
		if equal.SHA256Hex(page) != e.SHA256 {
			out.Close()
			return fmt.Errorf("page %d changed while it was captured", e.Page)
		}
		if _, err := w.Write(page); err != nil {
			out.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func ReadManifest(dir string) (equal.Manifest, error) {
	f, err := os.Open(filepath.Join(dir, ManifestFile))
	if err != nil {
		return equal.Manifest{}, err
	}
	defer f.Close()
	return equal.ParseManifest(f)
}

type PageError struct {
	Page    int
	Problem string
}

func (e *PageError) Error() string { return fmt.Sprintf("capture page %d: %s", e.Page, e.Problem) }

func Rebuild(dir, outPath string) (equal.Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return m, err
	}
	if err := m.CheckSequence(); err != nil {
		return m, err
	}
	if m.PageSize < dbfile.MinPageSize || m.PageSize > dbfile.MaxPageSize || m.PageSize&(m.PageSize-1) != 0 {
		return m, fmt.Errorf("manifest page size %d is not a valid page size", m.PageSize)
	}
	for _, suffix := range []string{"-wal", "-journal", "-shm"} {
		if _, err := os.Stat(outPath + suffix); err == nil {
			return m, fmt.Errorf("%s%s exists; SQLite would apply it to the rebuilt file", outPath, suffix)
		}
	}
	in, err := os.Open(filepath.Join(dir, PagesFile))
	if err != nil {
		return m, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return m, err
	}
	want := int64(m.PageCount()) * int64(m.PageSize)
	if info.Size() != want {
		return m, fmt.Errorf("%s holds %d bytes, the manifest lists %d pages of %d bytes (%d bytes)", PagesFile, info.Size(), m.PageCount(), m.PageSize, want)
	}
	tmp := outPath + ".rebuild-tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return m, err
	}
	fail := func(err error) (equal.Manifest, error) {
		out.Close()
		os.Remove(tmp)
		return m, err
	}
	reader := bufio.NewReader(in)
	page := make([]byte, m.PageSize)
	for _, e := range m.Entries {
		if _, err := io.ReadFull(reader, page); err != nil {
			return fail(err)
		}
		if equal.SHA256Hex(page) != e.SHA256 {
			return fail(&PageError{Page: e.Page, Problem: "image sha256 does not match the manifest"})
		}
		if _, err := out.Write(page); err != nil {
			return fail(err)
		}
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return m, err
	}
	return m, os.Rename(tmp, outPath)
}
