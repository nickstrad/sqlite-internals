package walfile

import (
	"bufio"
	"fmt"
	"strings"
)

// ShowwalFrame holds one frame line decoded from SQLite's in-tree showwal tool
// (tool/showwal.c:236).
type ShowwalFrame struct {
	Number           int
	Page             uint32
	CommitSize       uint32
	Salt1            uint32
	Salt2            uint32
	StoredChecksum   Checksum
	ChecksumAgrees   bool
	ExpectedChecksum Checksum
}

// ShowwalListing holds the parsed header summary and frame lines printed by
// running `showwal <file>`.
type ShowwalListing struct {
	PageSize        int
	AvailableFrames int
	Frames          []ShowwalFrame
}

// ParseShowwal parses stdout from SQLite's pinned showwal utility.
// showwal checks the running checksum chain and prints " should be ..." on
// mismatches, but does not validate salts (tool/showwal.c:236, 567).
func ParseShowwal(text string) (ShowwalListing, error) {
	var listing ShowwalListing
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "Pagesize: "):
			if _, err := fmt.Sscanf(line, "Pagesize: %d", &listing.PageSize); err != nil {
				return listing, fmt.Errorf("showwal line %q: %w", line, err)
			}
		case strings.HasPrefix(line, "Available pages: 1.."):
			if _, err := fmt.Sscanf(line, "Available pages: 1..%d", &listing.AvailableFrames); err != nil {
				return listing, fmt.Errorf("showwal line %q: %w", line, err)
			}
		case strings.HasPrefix(line, "Frame ") && !strings.Contains(line, "(offsets"):
			frame, err := parseShowwalFrame(line)
			if err != nil {
				return listing, err
			}
			listing.Frames = append(listing.Frames, frame)
		}
	}
	if err := scanner.Err(); err != nil {
		return listing, err
	}
	if listing.PageSize == 0 {
		return listing, fmt.Errorf("showwal output has no Pagesize line")
	}
	return listing, nil
}

func parseShowwalFrame(line string) (ShowwalFrame, error) {
	var frame ShowwalFrame
	main, expected, hasExpected := strings.Cut(line, " should be ")
	_, err := fmt.Sscanf(main, "Frame %d: %d %d 0x%x,%x 0x%x,%x",
		&frame.Number, &frame.Page, &frame.CommitSize, &frame.Salt1, &frame.Salt2,
		&frame.StoredChecksum[0], &frame.StoredChecksum[1])
	if err != nil {
		return frame, fmt.Errorf("showwal line %q: %w", line, err)
	}
	frame.ChecksumAgrees = !hasExpected
	frame.ExpectedChecksum = frame.StoredChecksum
	if hasExpected {
		if _, err := fmt.Sscanf(expected, "0x%x,%x", &frame.ExpectedChecksum[0], &frame.ExpectedChecksum[1]); err != nil {
			return frame, fmt.Errorf("showwal line %q: %w", line, err)
		}
	}
	return frame, nil
}

// CompareWithShowwal cross-checks f against a ShowwalListing from SQLite's
// in-tree showwal tool, returning one description per discrepancy. Every frame
// that walfile considers valid must also have a matching checksum in showwal.
func (f *File) CompareWithShowwal(listing ShowwalListing) []string {
	var problems []string
	if listing.PageSize != f.PageSize() {
		problems = append(problems, fmt.Sprintf("page size: showwal %d, walfile %d", listing.PageSize, f.PageSize()))
	}
	if listing.AvailableFrames != len(f.Frames) || len(listing.Frames) != len(f.Frames) {
		problems = append(problems, fmt.Sprintf("frame count: showwal %d (%d listed), walfile %d", listing.AvailableFrames, len(listing.Frames), len(f.Frames)))
	}
	for i := 0; i < min(len(listing.Frames), len(f.Frames)); i++ {
		s, w := listing.Frames[i], f.Frames[i]
		if s.Number != w.Number || s.Page != w.Page || s.CommitSize != w.CommitSize || s.Salt1 != w.Salt1 || s.Salt2 != w.Salt2 || s.StoredChecksum != w.StoredChecksum {
			problems = append(problems, fmt.Sprintf("frame %d: showwal page=%d commit=%d salts=%08x,%08x checksum=%s; walfile page=%d commit=%d salts=%08x,%08x checksum=%s",
				w.Number, s.Page, s.CommitSize, s.Salt1, s.Salt2, s.StoredChecksum, w.Page, w.CommitSize, w.Salt1, w.Salt2, w.StoredChecksum))
		}
		if i < f.ValidFrames && !s.ChecksumAgrees {
			problems = append(problems, fmt.Sprintf("frame %d: walfile says valid, showwal computes checksum %s", w.Number, s.ExpectedChecksum))
		}
		if i == f.ValidFrames && w.Problem == FrameChecksumMismatch && s.ChecksumAgrees {
			problems = append(problems, fmt.Sprintf("frame %d: walfile says checksum mismatch, showwal says the checksum agrees", w.Number))
		}
	}
	return problems
}
