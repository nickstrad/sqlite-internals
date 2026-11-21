package equal

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ByteRange is a half-open file byte offset range [Start, End) ignored during
// an E2Pages comparison.
type ByteRange struct {
	Start int
	End   int
}

// Contains reports whether file offset lies within [r.Start, r.End).
func (r ByteRange) Contains(offset int) bool { return offset >= r.Start && offset < r.End }

// String formats r using inclusive byte offsets (for example "24-27" or "43").
func (r ByteRange) String() string {
	if r.End-r.Start == 1 {
		return fmt.Sprintf("%d", r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End-1)
}

// DeclaredHeaderIgnore lists the volatile page-1 header byte ranges ignored by
// E2Pages: bytes 24-27 (file change counter, pager.c:3130-3143), bytes 40-43
// (schema cookie, incremented by sqlite3_backup_step at backup.c:448 and by
// sqlite3RunVacuum at vacuum.c:143-427), and bytes 92-99 (version-valid-for
// and SQLite library version). Header bytes 18-19 (write/read format version)
// are deliberately NOT ignored because they distinguish WAL from rollback mode.
var DeclaredHeaderIgnore = []ByteRange{{24, 28}, {40, 44}, {92, 100}}

// FormatRanges formats a slice of ByteRange values as a comma-separated list.
func FormatRanges(ranges []ByteRange) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

func formatOffsets(offsets []int) string {
	parts := make([]string, len(offsets))
	for i, o := range offsets {
		parts[i] = fmt.Sprintf("%d", o)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ValidateRanges verifies that every range has Start >= 0 and End > Start.
func ValidateRanges(ranges []ByteRange) error {
	for _, r := range ranges {
		if r.Start < 0 || r.End <= r.Start {
			return fmt.Errorf("invalid ignored byte range %d..%d", r.Start, r.End)
		}
	}
	return nil
}

func ignored(ranges []ByteRange, offset int) bool {
	for _, r := range ranges {
		if r.Contains(offset) {
			return true
		}
	}
	return false
}

// CompareImagesIgnoring compares raw database images a and b page by page at
// level E2Pages, allowing byte differences only at file offsets covered by
// ignore. When the images are equal outside ignore, Result.IgnoredDiffering
// records every file offset inside ignore where a and b differed.
func CompareImagesIgnoring(a, b []byte, pageSize int, ignore []ByteRange) (Result, error) {
	if err := ValidateRanges(ignore); err != nil {
		return Result{}, err
	}
	if pageSize <= 0 {
		return Result{}, fmt.Errorf("page size %d: an E2 comparison needs the page size", pageSize)
	}
	ranges := append([]ByteRange(nil), ignore...)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	r := Result{Level: E2Pages, SHA256A: SHA256Hex(a), SHA256B: SHA256Hex(b), SizeA: int64(len(a)), SizeB: int64(len(b)), PageSize: pageSize, Ignored: ranges}
	pages := (max(len(a), len(b)) + pageSize - 1) / pageSize
	for p := range pages {
		pageA, pageB := pageOf(a, p, pageSize), pageOf(b, p, pageSize)
		if bytes.Equal(pageA, pageB) {
			continue
		}
		if len(pageA) != len(pageB) || !equalOutside(pageA, pageB, p*pageSize, ranges, &r.IgnoredDiffering) {
			if r.FirstDifferingPage == 0 {
				r.FirstDifferingPage = p + 1
			}
			r.DifferingPages++
		}
	}
	r.Equal = r.DifferingPages == 0 && r.SizeA == r.SizeB
	if !r.Equal {
		// Only report IgnoredDiffering when every difference fell inside ignore.
		r.IgnoredDiffering = nil
	}
	return r, nil
}

func equalOutside(pageA, pageB []byte, base int, ranges []ByteRange, differing *[]int) bool {
	var inIgnored []int
	for i := range pageA {
		if pageA[i] == pageB[i] {
			continue
		}
		if !ignored(ranges, base+i) {
			return false
		}
		inIgnored = append(inIgnored, base+i)
	}
	*differing = append(*differing, inIgnored...)
	return true
}

// ComparePages compares raw database images a and b at level E2Pages using
// DeclaredHeaderIgnore.
func ComparePages(a, b []byte, pageSize int) (Result, error) {
	return CompareImagesIgnoring(a, b, pageSize, DeclaredHeaderIgnore)
}

// ComparePageFiles reads database files a and b from disk and compares them
// with ComparePages. Do not call on files open by SQLite in the same process.
func ComparePageFiles(a, b string, pageSize int) (Result, error) {
	imageA, err := os.ReadFile(a)
	if err != nil {
		return Result{}, err
	}
	imageB, err := os.ReadFile(b)
	if err != nil {
		return Result{}, err
	}
	return ComparePages(imageA, imageB, pageSize)
}
