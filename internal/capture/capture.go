// Package capture implements Rebuild Ladder rung R1: capturing every page of
// a closed SQLite database into raw page images plus a per-page hash manifest,
// and rebuilding a verified database file from that capture alone.
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

// Standard file names written inside a rung R1 capture directory.
const (
	// ManifestFile holds the serialized equal.Manifest text listing every page's role and SHA-256.
	ManifestFile = "manifest.txt"
	// PagesFile holds the raw page images concatenated in 1..N page order.
	PagesFile = "pages.bin"
)

// ErrNotAtRest is wrapped by NotAtRestError when a database has uncheckpointed
// WAL frames or a hot rollback journal.
var ErrNotAtRest = errors.New("database is not at rest")

// NotAtRestError explains why dbPath cannot safely be captured as a standalone file.
type NotAtRestError struct {
	Path   string
	Reason string
}

// Error formats the sentinel error, database path, and specific sidecar reason.
func (e *NotAtRestError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrNotAtRest, e.Path, e.Reason)
}

// Unwrap returns ErrNotAtRest for errors.Is matching.
func (e *NotAtRestError) Unwrap() error { return ErrNotAtRest }

// CheckAtRest verifies that dbPath is complete on its own: no non-empty -wal
// file and no hot -journal file. A zero-byte -wal (after TRUNCATE checkpoint or
// PERSIST_WAL) and a zero-byte or zero-header -journal (TRUNCATE/PERSIST modes)
// are accepted as at rest.
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

// hotJournal mirrors SQLite's hasHotJournal test (src/pager.c:5180): a rollback
// journal is hot when it exists, is non-empty, and its first header byte is
// non-zero (PERSIST mode zeroes the header on commit).
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

// BuildManifest runs a complete page census on f and hashes every page 1..N.
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

// Pages captures a closed database at dbPath into dir as pages.bin plus
// manifest.txt. It refuses databases that are not at rest or whose byte size
// disagrees with PageCount*PageSize, removes any prior capture in dir, writes
// temporary .capture-tmp files, re-checks at-rest status, size, and mtime, and
// renames the files into place only if every check passes so a failed capture
// leaves no partial or stale capture behind.
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
	pagesPath, manifestPath := filepath.Join(dir, PagesFile), filepath.Join(dir, ManifestFile)
	pagesTmp, manifestTmp := pagesPath+captureTmpSuffix, manifestPath+captureTmpSuffix
	removeAll := func(paths ...string) {
		for _, p := range paths {
			os.Remove(p)
		}
	}
	for _, p := range []string{manifestPath, pagesPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return equal.Manifest{}, err
		}
	}
	if err := writeCapture(f, dbPath, before, m, pagesTmp, manifestTmp); err != nil {
		removeAll(pagesTmp, manifestTmp)
		return equal.Manifest{}, err
	}
	if err := os.Rename(pagesTmp, pagesPath); err != nil {
		removeAll(pagesTmp, manifestTmp)
		return equal.Manifest{}, err
	}
	if err := os.Rename(manifestTmp, manifestPath); err != nil {
		removeAll(pagesPath, manifestTmp)
		return equal.Manifest{}, err
	}
	return m, nil
}

// captureTmpSuffix is appended to pages.bin and manifest.txt until the final
// post-capture stability checks pass.
const captureTmpSuffix = ".capture-tmp"

// afterCaptureWritten is a test hook invoked after the temp files are written
// and before the final at-rest and stat checks run.
var afterCaptureWritten = func(dbPath string) {}

// writeCapture streams pages.bin and manifest.txt to temporary paths and then
// verifies the source database remained at rest with unchanged size and mtime.
func writeCapture(f *dbfile.File, dbPath string, before os.FileInfo, m equal.Manifest, pagesPath, manifestPath string) error {
	if err := writePages(f, pagesPath, m); err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, []byte(m.Text()), 0o644); err != nil {
		return err
	}
	afterCaptureWritten(dbPath)
	if err := CheckAtRest(dbPath); err != nil {
		return fmt.Errorf("database changed during the capture: %w", err)
	}
	after, err := os.Stat(dbPath)
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("%s: database changed during the capture", dbPath)
	}
	return nil
}

// writePages writes every page image in manifest order to path, re-hashing each
// page as it goes so a concurrent overwrite fails immediately.
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

// ReadManifest opens and parses ManifestFile inside capture directory dir.
func ReadManifest(dir string) (equal.Manifest, error) {
	f, err := os.Open(filepath.Join(dir, ManifestFile))
	if err != nil {
		return equal.Manifest{}, err
	}
	defer f.Close()
	return equal.ParseManifest(f)
}

// PageError reports a page image inside pages.bin that failed manifest verification.
type PageError struct {
	Page    int
	Problem string
}

// Error formats the offending capture page number and verification failure.
func (e *PageError) Error() string { return fmt.Sprintf("capture page %d: %s", e.Page, e.Problem) }

// Rebuild reconstructs a database file at outPath from a capture directory
// alone. It validates the manifest sequence (requiring at least one page),
// refuses stale sidecar files next to outPath, verifies every page's SHA-256
// while streaming into a temporary file, fsyncs, runs a full page census on
// the rebuilt image to confirm it agrees with the manifest, and atomically
// renames into place; on any failure no output or temp file is left behind.
func Rebuild(dir, outPath string) (equal.Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return m, err
	}
	if err := m.CheckSequence(); err != nil {
		return m, err
	}
	if m.PageCount() == 0 {
		return m, errors.New("the manifest lists no pages; a database has at least one")
	}
	if m.PageSize < dbfile.MinPageSize || m.PageSize > dbfile.MaxPageSize || m.PageSize&(m.PageSize-1) != 0 {
		return m, fmt.Errorf("manifest page size %d is not a valid page size", m.PageSize)
	}
	// Refuse to write beside an existing -wal, -journal, or -shm because SQLite
	// would replay or roll back that sidecar against the freshly rebuilt file.
	for _, suffix := range []string{"-wal", "-journal", "-shm"} {
		if _, err := os.Stat(outPath + suffix); err == nil {
			return m, fmt.Errorf("%s%s exists; SQLite would apply it to the rebuilt file", outPath, suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return m, err
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
	if err := checkRebuilt(tmp, m); err != nil {
		os.Remove(tmp)
		return m, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return m, err
	}
	return m, nil
}

// checkRebuilt runs dbfile's page census on the rebuilt temp file and compares
// the resulting manifest against m, catching coordinated truncation (where both
// pages.bin and manifest.txt drop trailing pages) and tampered page roles.
func checkRebuilt(path string, m equal.Manifest) error {
	f, err := dbfile.Open(path)
	if err != nil {
		return fmt.Errorf("the rebuilt file is not a database the page census accepts: %w", err)
	}
	defer f.Close()
	if int64(f.PageCount())*int64(f.PageSize()) != f.FileSize() {
		return fmt.Errorf("the rebuilt file holds %d bytes but its header describes %d pages of %d bytes", f.FileSize(), f.PageCount(), f.PageSize())
	}
	got, err := BuildManifest(f)
	if err != nil {
		return fmt.Errorf("the rebuilt file is not a database the page census accepts: %w", err)
	}
	if diff := equal.CompareManifests(m, got); !diff.Equal {
		if diff.FirstDifferingPage == 0 {
			return fmt.Errorf("the rebuilt file's census disagrees with the manifest: %s", diff.Reason)
		}
		return &PageError{Page: diff.FirstDifferingPage, Problem: "the manifest disagrees with the rebuilt file's census: " + diff.Reason}
	}
	return nil
}

func VerifyImage(m equal.Manifest, image []byte) ([]int, error) {
	if m.PageSize <= 0 {
		return nil, fmt.Errorf("manifest page size %d", m.PageSize)
	}
	if err := m.CheckSequence(); err != nil {
		return nil, err
	}
	if len(image) != m.PageCount()*m.PageSize {
		return nil, fmt.Errorf("image is %d bytes, the manifest lists %d pages of %d bytes", len(image), m.PageCount(), m.PageSize)
	}
	var differing []int
	for i, entry := range m.Entries {
		if equal.SHA256Hex(image[i*m.PageSize:(i+1)*m.PageSize]) != entry.SHA256 {
			differing = append(differing, entry.Page)
		}
	}
	return differing, nil
}
