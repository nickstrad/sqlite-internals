// Package equal implements the course's Rebuild Ladder equality checks and
// per-page hash manifests.
//
// Chapter 2 introduces level E1 (exact byte equality via SHA-256 and page-by-page
// diffing) and the text manifest format; Chapter 3 extends the package with
// levels E2 (page comparison ignoring volatile header bytes) and E3 (logical
// content hash).
package equal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// Level identifies a rung on the course's three-level equality ladder.
type Level int

// Equality levels supported by this package.
const (
	// E1Bytes checks exact byte-for-byte equality via SHA-256 and locates the first differing page.
	E1Bytes   Level = 1
	E2Pages   Level = 2
	E3Logical Level = 3
)

// String returns the human-readable label for equality level l.
func (l Level) String() string {
	switch l {
	case E1Bytes:
		return "E1 bytes"
	case E2Pages:
		return "E2 pages"
	case E3Logical:
		return "E3 logical"
	}
	return fmt.Sprintf("E%d", int(l))
}

// Result summarizes the outcome of comparing two database images or files at a
// given Level, including both SHA-256 digests, sizes, and differing-page counts.
type Result struct {
	Level              Level
	Equal              bool
	SHA256A            string
	SHA256B            string
	SizeA              int64
	SizeB              int64
	PageSize           int
	FirstDifferingPage int
	DifferingPages     int
	Ignored            []ByteRange
	IgnoredDiffering   []int
}

// String formats r as a single-line equality verdict for experiment output.
func (r Result) String() string {
	if r.Equal && len(r.IgnoredDiffering) > 0 {
		return fmt.Sprintf("%s: equal ignoring header bytes %s (bytes differ only at %s)", r.Level, FormatRanges(r.Ignored), formatOffsets(r.IgnoredDiffering))
	}
	if r.Equal {
		return fmt.Sprintf("%s: equal (sha256 %s)", r.Level, r.SHA256A)
	}
	text := fmt.Sprintf("%s: differ", r.Level)
	if r.FirstDifferingPage > 0 {
		text += fmt.Sprintf(", first differing page %d, %d page(s) differ", r.FirstDifferingPage, r.DifferingPages)
	}
	if r.SizeA != r.SizeB {
		text += fmt.Sprintf(", sizes %d and %d bytes", r.SizeA, r.SizeB)
	}
	return text
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReaderSHA256 streams r to EOF, returning its lowercase hex SHA-256 digest and
// byte count.
func ReaderSHA256(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FileSHA256 hashes the contents of path and returns its lowercase hex SHA-256 digest.
// Do not call on a file currently open by an SQLite connection in the same process.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum, _, err := ReaderSHA256(f)
	return sum, err
}

// CompareImages compares two raw database images at level E1Bytes. When they
// differ and pageSize > 0, it walks every page slot to record the 1-based first
// differing page and total differing page count.
func CompareImages(a, b []byte, pageSize int) Result {
	r := Result{Level: E1Bytes, SHA256A: SHA256Hex(a), SHA256B: SHA256Hex(b), SizeA: int64(len(a)), SizeB: int64(len(b)), PageSize: pageSize}
	r.Equal = r.SHA256A == r.SHA256B
	if r.Equal || pageSize <= 0 {
		return r
	}
	pages := (max(len(a), len(b)) + pageSize - 1) / pageSize
	for p := range pages {
		if !bytes.Equal(pageOf(a, p, pageSize), pageOf(b, p, pageSize)) {
			if r.FirstDifferingPage == 0 {
				r.FirstDifferingPage = p + 1
			}
			r.DifferingPages++
		}
	}
	return r
}

func pageOf(image []byte, index, pageSize int) []byte {
	start := index * pageSize
	if start >= len(image) {
		return nil
	}
	return image[start:min(start+pageSize, len(image))]
}

// CompareFiles reads both files from disk and compares them with CompareImages.
func CompareFiles(a, b string, pageSize int) (Result, error) {
	imageA, err := os.ReadFile(a)
	if err != nil {
		return Result{}, err
	}
	imageB, err := os.ReadFile(b)
	if err != nil {
		return Result{}, err
	}
	return CompareImages(imageA, imageB, pageSize), nil
}
