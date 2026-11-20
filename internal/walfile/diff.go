package walfile

import (
	"fmt"
	"strings"
)

// Region classifies a byte offset inside a WAL file into its structural field
// (WAL header field, frame header field, page image, or trailing length delta).
type Region int

// Structural regions of a WAL file (src/wal.c:34-57).
const (
	RegionHeaderMagic Region = iota
	RegionHeaderVersion
	RegionHeaderPageSize
	RegionHeaderCheckpointSequence
	RegionHeaderSalt
	RegionHeaderChecksum
	RegionFramePageNumber
	RegionFrameCommitSize
	RegionFrameSalt
	RegionFrameChecksum
	RegionPageData
	RegionBeyondShorterFile
	regionCount
)

// String returns a human-readable label for r.
func (r Region) String() string {
	switch r {
	case RegionHeaderMagic:
		return "header magic"
	case RegionHeaderVersion:
		return "header version"
	case RegionHeaderPageSize:
		return "header page size"
	case RegionHeaderCheckpointSequence:
		return "header checkpoint sequence"
	case RegionHeaderSalt:
		return "header salts"
	case RegionHeaderChecksum:
		return "header checksum"
	case RegionFramePageNumber:
		return "frame page number"
	case RegionFrameCommitSize:
		return "frame commit size"
	case RegionFrameSalt:
		return "frame salts"
	case RegionFrameChecksum:
		return "frame checksums"
	case RegionPageData:
		return "page data"
	case RegionBeyondShorterFile:
		return "bytes beyond the end of the shorter file"
	}
	return fmt.Sprintf("Region(%d)", int(r))
}

// RegionAt maps a byte offset in a WAL with the given pageSize to its Region.
func RegionAt(offset int64, pageSize int) Region {
	if offset < HeaderSize {
		switch {
		case offset < 4:
			return RegionHeaderMagic
		case offset < 8:
			return RegionHeaderVersion
		case offset < 12:
			return RegionHeaderPageSize
		case offset < 16:
			return RegionHeaderCheckpointSequence
		case offset < 24:
			return RegionHeaderSalt
		}
		return RegionHeaderChecksum
	}
	if pageSize <= 0 {
		return RegionPageData
	}
	within := (offset - HeaderSize) % int64(pageSize+FrameHeaderSize)
	switch {
	case within < 4:
		return RegionFramePageNumber
	case within < 8:
		return RegionFrameCommitSize
	case within < 16:
		return RegionFrameSalt
	case within < FrameHeaderSize:
		return RegionFrameChecksum
	}
	return RegionPageData
}

// ByteComparison summarizes every byte difference between two WAL files broken
// down by Region.
type ByteComparison struct {
	SizeA          int64
	SizeB          int64
	DifferingBytes int64
	ByRegion       [regionCount]int64
}

// Identical reports whether the two WAL byte slices have equal length and zero
// differing bytes.
func (c ByteComparison) Identical() bool {
	return c.SizeA == c.SizeB && c.DifferingBytes == 0
}

// OnlyIn reports whether the two WAL files have the same size and every
// differing byte falls within the listed regions (used to verify that two
// deterministic runs differ only in random salts and dependent checksums).
func (c ByteComparison) OnlyIn(regions ...Region) bool {
	allowed := map[Region]bool{}
	for _, r := range regions {
		allowed[r] = true
	}
	for r := Region(0); r < regionCount; r++ {
		if c.ByRegion[r] > 0 && !allowed[r] {
			return false
		}
	}
	return c.SizeA == c.SizeB
}

// String formats the comparison summary with per-region differing byte counts.
func (c ByteComparison) String() string {
	if c.Identical() {
		return fmt.Sprintf("byte-identical (%d bytes)", c.SizeA)
	}
	var parts []string
	for r := Region(0); r < regionCount; r++ {
		if c.ByRegion[r] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", r, c.ByRegion[r]))
		}
	}
	text := fmt.Sprintf("%d differing bytes", c.DifferingBytes)
	if c.SizeA != c.SizeB {
		text += fmt.Sprintf(", sizes %d and %d", c.SizeA, c.SizeB)
	}
	if len(parts) > 0 {
		text += ": " + strings.Join(parts, ", ")
	}
	return text
}

// CompareBytes compares two raw WAL byte slices byte by byte and attributes
// each difference to its WAL Region.
func CompareBytes(a, b []byte, pageSize int) ByteComparison {
	result := ByteComparison{SizeA: int64(len(a)), SizeB: int64(len(b))}
	shorter := min(len(a), len(b))
	longer := max(len(a), len(b))
	for i := 0; i < longer; i++ {
		if i >= shorter {
			result.DifferingBytes++
			result.ByRegion[RegionBeyondShorterFile]++
			continue
		}
		if a[i] == b[i] {
			continue
		}
		result.DifferingBytes++
		result.ByRegion[RegionAt(int64(i), pageSize)]++
	}
	return result
}
