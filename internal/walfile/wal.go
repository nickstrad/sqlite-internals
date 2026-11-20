package walfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
)

const (
	HeaderSize        = 32
	FrameHeaderSize   = 24
	MagicLittleEndian = 0x377f0682
	MagicBigEndian    = 0x377f0683
	FormatVersion     = 3007000
	MinPageSize       = 512
	MaxPageSize       = 65536
)

type Header struct {
	Magic              uint32
	Version            uint32
	PageSize           uint32
	CheckpointSequence uint32
	Salt1              uint32
	Salt2              uint32
	Checksum           Checksum
}

func (h Header) BigEndianChecksums() bool { return h.Magic&1 == 1 }

func (h Header) ChecksumByteOrder() string {
	if h.BigEndianChecksums() {
		return "big-endian"
	}
	return "little-endian"
}

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

type FrameProblem int

const (
	FrameValid FrameProblem = iota
	FrameSaltMismatch
	FramePageZero
	FrameChecksumMismatch
	FrameAfterInvalid
)

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

func (f Frame) Valid() bool    { return f.Problem == FrameValid }
func (f Frame) IsCommit() bool { return f.CommitSize != 0 }

type Tuple struct {
	Page       uint32
	PageSHA256 string
	CommitSize uint32
}

func (t Tuple) String() string {
	return fmt.Sprintf("page=%d commit-size=%d sha256=%s", t.Page, t.CommitSize, t.PageSHA256)
}

type Commit struct {
	FirstFrame int
	LastFrame  int
	Pages      uint32
}

func (c Commit) Frames() int { return c.LastFrame - c.FirstFrame + 1 }

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

func (f *File) PageSize() int { return int(f.Header.PageSize) }

func (f *File) FrameSize() int { return int(f.Header.PageSize) + FrameHeaderSize }

func (f *File) StopReason() FrameProblem {
	if f.ValidFrames < len(f.Frames) {
		return f.Frames[f.ValidFrames].Problem
	}
	return FrameValid
}

func (f *File) CommittedFrames() []Frame { return f.Frames[:f.LastCommitFrame] }

func (f *File) UncommittedValidFrames() []Frame {
	return f.Frames[f.LastCommitFrame:f.ValidFrames]
}

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

func (f *File) DatabasePages() uint32 {
	if f.LastCommitFrame == 0 {
		return 0
	}
	return f.Frames[f.LastCommitFrame-1].CommitSize
}

func (f *File) PageImage(frameNumber int) ([]byte, error) {
	if frameNumber < 1 || frameNumber > len(f.Frames) {
		return nil, fmt.Errorf("frame %d is not in this WAL (%d frames)", frameNumber, len(f.Frames))
	}
	start := f.Frames[frameNumber-1].Offset + FrameHeaderSize
	return f.data[start : start+int64(f.PageSize())], nil
}

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

func ParseFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

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
	if header.Version != FormatVersion {
		return nil, &HeaderError{Kind: ErrUnsupportedVersion, Reason: fmt.Sprintf("version %d, want %d", header.Version, FormatVersion)}
	}
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
