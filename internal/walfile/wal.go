// Package walfile parses SQLite write-ahead log (-wal) and wal-index (-shm)
// files in plain Go without linking SQLite, replays committed WAL frames onto a
// base database file, compares WAL files by structural region or frame tuples,
// cross-checks against SQLite's showwal utility, and copies live database files
// through a child process so POSIX advisory locks are never dropped.
package walfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
)

// On-disk WAL format constants (src/wal.c:34-57, 477-491).
const (
	// HeaderSize is the fixed 32-byte WAL file header length (WAL_HDRSIZE).
	HeaderSize = 32
	// FrameHeaderSize is the fixed 24-byte header preceding each page image
	// (WAL_FRAME_HDRSIZE).
	FrameHeaderSize = 24
	// MagicLittleEndian selects little-endian 32-bit word reads for checksums
	// (WAL_MAGIC, src/wal.c:491).
	MagicLittleEndian = 0x377f0682
	// MagicBigEndian selects big-endian 32-bit word reads for checksums
	// (WAL_MAGIC | 1, src/wal.c:482-491).
	MagicBigEndian = 0x377f0683
	// FormatVersion is the only WAL file format version SQLite 3.53.4 accepts
	// (WAL_MAX_VERSION, src/wal.c:1472).
	FormatVersion = 3007000
	// MinPageSize is the smallest valid SQLite page size in bytes.
	MinPageSize = 512
	// MaxPageSize is the largest valid SQLite page size in bytes.
	MaxPageSize = 65536
)

// Header holds the eight big-endian 32-bit fields of the 32-byte WAL header
// (src/wal.c:34-44).
type Header struct {
	Magic              uint32
	Version            uint32
	PageSize           uint32
	CheckpointSequence uint32
	Salt1              uint32
	Salt2              uint32
	Checksum           Checksum
}

// BigEndianChecksums reports whether checksums in this WAL treat 32-bit input
// words as big-endian (indicated by bit 0 of Magic, src/wal.c:70-72, 1454).
func (h Header) BigEndianChecksums() bool { return h.Magic&1 == 1 }

// ChecksumByteOrder names the word byte order used by the checksum algorithm.
func (h Header) ChecksumByteOrder() string {
	if h.BigEndianChecksums() {
		return "big-endian"
	}
	return "little-endian"
}

// String formats all WAL header fields in a single readable line.
func (h Header) String() string {
	return fmt.Sprintf("magic=0x%08x (%s checksums) version=%d page-size=%d checkpoint-seq=%d salt-1=0x%08x salt-2=0x%08x checksum=%s",
		h.Magic, h.ChecksumByteOrder(), h.Version, h.PageSize, h.CheckpointSequence, h.Salt1, h.Salt2, h.Checksum)
}

func decodeHeader(data []byte) Header {
	be := binary.BigEndian
	return Header{
		Magic:              be.Uint32(data[0:]),
		Version:            be.Uint32(data[4:]),
		PageSize:           be.Uint32(data[8:]),
		CheckpointSequence: be.Uint32(data[12:]),
		Salt1:              be.Uint32(data[16:]),
		Salt2:              be.Uint32(data[20:]),
		Checksum:           Checksum{be.Uint32(data[24:]), be.Uint32(data[28:])},
	}
}

// FrameProblem classifies why a WAL frame failed validation or was stopped
// behind an earlier invalid frame (src/wal.c:1000-1053, walDecodeFrame).
type FrameProblem int

// Frame validation outcomes in the exact check order SQLite uses.
const (
	FrameValid FrameProblem = iota
	FrameSaltMismatch
	FramePageZero
	FrameChecksumMismatch
	FrameAfterInvalid
)

// String returns a human-readable explanation of p.
func (p FrameProblem) String() string {
	switch p {
	case FrameValid:
		return "valid"
	case FrameSaltMismatch:
		return "salt mismatch (frame from an earlier WAL generation, or rewritten in place by an open transaction)"
	case FramePageZero:
		return "page number 0"
	case FrameChecksumMismatch:
		return "checksum mismatch (running checksum chain broken)"
	case FrameAfterInvalid:
		return "follows an invalid frame"
	}
	return fmt.Sprintf("FrameProblem(%d)", int(p))
}

// Frame represents one whole frame slot in a WAL file: its 1-based slot number,
// file offset, decoded 24-byte header fields, page SHA-256, and validation state.
type Frame struct {
	Number           int
	Offset           int64
	Page             uint32
	CommitSize       uint32
	Salt1            uint32
	Salt2            uint32
	StoredChecksum   Checksum
	PageSHA256       string
	SaltsMatchHeader bool
	Problem          FrameProblem
}

// Valid reports whether f passed salt, page-number, and cumulative-checksum
// validation as part of the unbroken valid prefix of the WAL.
func (f Frame) Valid() bool { return f.Problem == FrameValid }

// IsCommit reports whether f marks a transaction commit (non-zero database size
// in pages, src/wal.c:52-53, 1516).
func (f Frame) IsCommit() bool { return f.CommitSize != 0 }

// Tuple is the run-independent projection of a WAL frame: page number, SHA-256
// of the page image, and commit size. Comparing tuples ignores random salts and
// checksums so two runs of a deterministic workload can be compared directly.
type Tuple struct {
	Page       uint32
	PageSHA256 string
	CommitSize uint32
}

// String formats a Tuple as key=value fields.
func (t Tuple) String() string {
	return fmt.Sprintf("page=%d commit-size=%d sha256=%s", t.Page, t.CommitSize, t.PageSHA256)
}

// Commit describes one committed transaction inside the valid prefix of a WAL:
// its inclusive 1-based frame range and the resulting database size in pages.
type Commit struct {
	FirstFrame int
	LastFrame  int
	Pages      uint32
}

// Frames returns the number of WAL frames belonging to c.
func (c Commit) Frames() int { return c.LastFrame - c.FirstFrame + 1 }

// File is a parsed WAL file holding the header, every whole frame slot present
// in the file, the lengths of the valid and committed prefixes, and any
// trailing bytes that do not form a complete frame.
type File struct {
	Size              int64
	Header            Header
	ComputedChecksum  Checksum
	Frames            []Frame
	ValidFrames       int
	LastCommitFrame   int
	PartialFrameBytes int64
	data              []byte
}

// PageSize returns the database page size recorded in the WAL header.
func (f *File) PageSize() int { return int(f.Header.PageSize) }

// FrameSize returns the total byte size of one frame (24-byte header + page).
func (f *File) FrameSize() int { return int(f.Header.PageSize) + FrameHeaderSize }

// StopReason returns the FrameProblem of the first invalid frame, or FrameValid
// when every whole frame slot in the WAL verified.
func (f *File) StopReason() FrameProblem {
	if f.ValidFrames < len(f.Frames) {
		return f.Frames[f.ValidFrames].Problem
	}
	return FrameValid
}

// CommittedFrames returns the valid prefix up to and including the last commit
// frame (empty if no commit frame exists).
func (f *File) CommittedFrames() []Frame { return f.Frames[:f.LastCommitFrame] }

// UncommittedValidFrames returns valid frames after LastCommitFrame that have
// no subsequent commit frame (for example, spilled frames from an open or
// rolled-back transaction whose checksums were never invalidated).
func (f *File) UncommittedValidFrames() []Frame {
	return f.Frames[f.LastCommitFrame:f.ValidFrames]
}

// Commits groups CommittedFrames into one Commit entry per commit frame.
func (f *File) Commits() []Commit {
	var commits []Commit
	first := 1
	for _, frame := range f.Frames[:f.LastCommitFrame] {
		if frame.IsCommit() {
			commits = append(commits, Commit{FirstFrame: first, LastFrame: frame.Number, Pages: frame.CommitSize})
			first = frame.Number + 1
		}
	}
	return commits
}

// DatabasePages returns the committed database size in pages from the last
// commit frame, or 0 if the WAL contains no committed transaction.
func (f *File) DatabasePages() uint32 {
	if f.LastCommitFrame == 0 {
		return 0
	}
	return f.Frames[f.LastCommitFrame-1].CommitSize
}

// PageImage returns the raw page bytes stored in 1-based frameNumber.
func (f *File) PageImage(frameNumber int) ([]byte, error) {
	if frameNumber < 1 || frameNumber > len(f.Frames) {
		return nil, fmt.Errorf("frame %d is not in this WAL (%d frames)", frameNumber, len(f.Frames))
	}
	start := f.Frames[frameNumber-1].Offset + FrameHeaderSize
	return f.data[start : start+int64(f.PageSize())], nil
}

// Tuples projects the WAL frames (or only the valid prefix when validOnly is
// true) to their salt-independent Tuple sequence.
func (f *File) Tuples(validOnly bool) []Tuple {
	frames := f.Frames
	if validOnly {
		frames = f.Frames[:f.ValidFrames]
	}
	tuples := make([]Tuple, 0, len(frames))
	for _, frame := range frames {
		tuples = append(tuples, Tuple{Page: frame.Page, PageSHA256: frame.PageSHA256, CommitSize: frame.CommitSize})
	}
	return tuples
}

// ParseFile reads path from disk and decodes it with Parse.
func ParseFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a raw WAL byte slice following SQLite's recovery rules in
// walIndexRecover (src/wal.c:1390-1614) and walDecodeFrame (src/wal.c:1000-1053).
// A 0-byte WAL is valid and empty (for example right after TRUNCATE); a bad
// header magic, page size, or checksum returns *HeaderError wrapping
// ErrWALIgnored; a version other than 3007000 returns ErrUnsupportedVersion.
func Parse(data []byte) (*File, error) {
	file := &File{Size: int64(len(data)), data: data}
	if len(data) == 0 {
		return file, nil
	}
	if len(data) < HeaderSize {
		return nil, ignored("file is %d bytes, shorter than the %d-byte WAL header", len(data), HeaderSize)
	}
	header := decodeHeader(data)
	file.Header = header
	// SQLite ignores the entire WAL when magic, page size, or header checksum
	// fails (src/wal.c:1447-1467, jumping to finished with zero frames).
	if header.Magic&0xfffffffe != MagicLittleEndian {
		return nil, ignored("magic 0x%08x is neither 0x%08x nor 0x%08x", header.Magic, MagicLittleEndian, MagicBigEndian)
	}
	pageSize := header.PageSize
	if pageSize < MinPageSize || pageSize > MaxPageSize || pageSize&(pageSize-1) != 0 {
		return nil, ignored("page size %d is not a power of two between %d and %d", pageSize, MinPageSize, MaxPageSize)
	}
	file.ComputedChecksum = sumWordPairs(header.BigEndianChecksums(), data[:HeaderSize-8], Checksum{})
	if file.ComputedChecksum != header.Checksum {
		return nil, ignored("header checksum %s does not match the computed %s", header.Checksum, file.ComputedChecksum)
	}
	// Version is checked after the header checksum, matching src/wal.c:1460-1475.
	if header.Version != FormatVersion {
		return nil, &HeaderError{Kind: ErrUnsupportedVersion, Reason: fmt.Sprintf("version %d, want %d", header.Version, FormatVersion)}
	}
	// SQLite processes only whole frame slots and ignores trailing partial bytes
	// (src/wal.c:1489).
	frameSize := int64(pageSize) + FrameHeaderSize
	slots := (int64(len(data)) - HeaderSize) / frameSize
	file.PartialFrameBytes = (int64(len(data)) - HeaderSize) % frameSize
	file.Frames = make([]Frame, 0, slots)
	chain := file.ComputedChecksum
	broken := false
	bigEndian := header.BigEndianChecksums()
	be := binary.BigEndian
	for i := int64(0); i < slots; i++ {
		offset := HeaderSize + i*frameSize
		frameHeader := data[offset : offset+FrameHeaderSize]
		page := data[offset+FrameHeaderSize : offset+frameSize]
		digest := sha256.Sum256(page)
		frame := Frame{
			Number:         int(i) + 1,
			Offset:         offset,
			Page:           be.Uint32(frameHeader[0:]),
			CommitSize:     be.Uint32(frameHeader[4:]),
			Salt1:          be.Uint32(frameHeader[8:]),
			Salt2:          be.Uint32(frameHeader[12:]),
			StoredChecksum: Checksum{be.Uint32(frameHeader[16:]), be.Uint32(frameHeader[20:])},
			PageSHA256:     hex.EncodeToString(digest[:]),
		}
		frame.SaltsMatchHeader = bytes.Equal(frameHeader[8:16], data[16:24])
		// Validate in walDecodeFrame's exact order (src/wal.c:1015-1045):
		// salts, then page != 0, then running checksum over header bytes 0..7
		// (not 16, despite the stale comment at src/wal.c:1033) plus the page.
		// Once any frame fails, every subsequent frame is ignored (src/wal.c:1511).
		switch {
		case broken:
			frame.Problem = FrameAfterInvalid
		case !frame.SaltsMatchHeader:
			frame.Problem = FrameSaltMismatch
		case frame.Page == 0:
			frame.Problem = FramePageZero
		default:
			next := sumWordPairs(bigEndian, frameHeader[:8], chain)
			next = sumWordPairs(bigEndian, page, next)
			if next != frame.StoredChecksum {
				frame.Problem = FrameChecksumMismatch
			} else {
				chain = next
			}
		}
		if frame.Problem != FrameValid {
			broken = true
		} else {
			file.ValidFrames = frame.Number
			if frame.IsCommit() {
				file.LastCommitFrame = frame.Number
			}
		}
		file.Frames = append(file.Frames, frame)
	}
	return file, nil
}

// EqualTuples reports whether a and b have identical Tuple elements in order.
// On mismatch it returns false and the 1-based frame number of the first
// difference; on equality it returns true and 0.
func EqualTuples(a, b []Tuple) (bool, int) {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false, i + 1
		}
	}
	if len(a) != len(b) {
		return false, n + 1
	}
	return true, 0
}
