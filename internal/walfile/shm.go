package walfile

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

const (
	IndexHeaderSize         = 48
	IndexHeaderCopies       = 2
	CheckpointInfoOffset    = IndexHeaderSize * IndexHeaderCopies
	CheckpointInfoSize      = 40
	IndexHeaderRegionSize   = CheckpointInfoOffset + CheckpointInfoSize
	ReadMarkCount           = 5
	ReadMarkNotUsed         = 0xffffffff
	IndexBlockSize          = 32768
	FramesPerIndexBlock     = 4096
	FramesInFirstIndexBlock = FramesPerIndexBlock - IndexHeaderRegionSize/4
	LockBytesOffset         = 120
	LockByteCount           = 8
	DeadManSwitchOffset     = LockBytesOffset + LockByteCount
	IndexFormatVersion      = 3007000
	indexChecksumLength     = 40
)

type IndexHeader struct {
	Version           uint32
	Change            uint32
	IsInit            uint8
	BigEndianChecksum uint8
	PageSizeField     uint16
	MaxFrame          uint32
	Pages             uint32
	FrameChecksum     Checksum
	Salt1             uint32
	Salt2             uint32
	Checksum          Checksum
	ChecksumOK        bool
}

func (h IndexHeader) PageSize() int {
	return int(h.PageSizeField&0xfe00) + int(h.PageSizeField&0x0001)<<16
}

func (h IndexHeader) String() string {
	return fmt.Sprintf("version=%d change=%d is-init=%d page-size=%d mxFrame=%d nPage=%d salt-1=0x%08x salt-2=0x%08x checksum-ok=%v",
		h.Version, h.Change, h.IsInit, h.PageSize(), h.MaxFrame, h.Pages, h.Salt1, h.Salt2, h.ChecksumOK)
}

type CheckpointInfo struct {
	Backfill          uint32
	ReadMarks         [ReadMarkCount]uint32
	BackfillAttempted uint32
}

func ReadMarkText(mark uint32) string {
	if mark == ReadMarkNotUsed {
		return "unused"
	}
	return fmt.Sprintf("%d", mark)
}

func (c CheckpointInfo) ReadMarksText() string {
	parts := make([]string, len(c.ReadMarks))
	for i, mark := range c.ReadMarks {
		parts[i] = fmt.Sprintf("[%d]=%s", i, ReadMarkText(mark))
	}
	return strings.Join(parts, " ")
}

func (c CheckpointInfo) String() string {
	return fmt.Sprintf("nBackfill=%d nBackfillAttempted=%d read-marks %s", c.Backfill, c.BackfillAttempted, c.ReadMarksText())
}

type Index struct {
	Size        int
	Initialized bool
	BigEndian   bool
	Headers     [IndexHeaderCopies]IndexHeader
	Checkpoint  CheckpointInfo
	order       binary.ByteOrder
	data        []byte
}

func (x *Index) Header() IndexHeader { return x.Headers[0] }

func (x *Index) HeadersAgree() bool {
	return x.Headers[0] == x.Headers[1]
}

func (x *Index) ByteOrderName() string {
	if x.BigEndian {
		return "big-endian"
	}
	return "little-endian"
}

func ParseIndexFile(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIndex(data)
}

func ParseIndex(data []byte) (*Index, error) {
	if len(data) < IndexHeaderRegionSize {
		return nil, fmt.Errorf("%w: %d bytes, need %d", ErrShortIndex, len(data), IndexHeaderRegionSize)
	}
	index := &Index{Size: len(data), data: data}
	switch {
	case binary.LittleEndian.Uint32(data) == IndexFormatVersion:
		index.order = binary.LittleEndian
	case binary.BigEndian.Uint32(data) == IndexFormatVersion:
		index.order = binary.BigEndian
		index.BigEndian = true
	case allZero(data[:IndexHeaderSize]):
		index.order = binary.LittleEndian
		return index, nil
	default:
		return nil, fmt.Errorf("%w: first word %08x", ErrIndexVersion, binary.BigEndian.Uint32(data))
	}
	index.Initialized = true
	for i := range index.Headers {
		index.Headers[i] = decodeIndexHeader(data[i*IndexHeaderSize:(i+1)*IndexHeaderSize], index.order, index.BigEndian)
	}
	info := data[CheckpointInfoOffset:]
	index.Checkpoint.Backfill = index.order.Uint32(info[0:])
	for i := range index.Checkpoint.ReadMarks {
		index.Checkpoint.ReadMarks[i] = index.order.Uint32(info[4+4*i:])
	}
	index.Checkpoint.BackfillAttempted = index.order.Uint32(info[32:])
	return index, nil
}

func decodeIndexHeader(raw []byte, order binary.ByteOrder, bigEndian bool) IndexHeader {
	h := IndexHeader{
		Version:           order.Uint32(raw[0:]),
		Change:            order.Uint32(raw[8:]),
		IsInit:            raw[12],
		BigEndianChecksum: raw[13],
		PageSizeField:     order.Uint16(raw[14:]),
		MaxFrame:          order.Uint32(raw[16:]),
		Pages:             order.Uint32(raw[20:]),
		FrameChecksum:     Checksum{order.Uint32(raw[24:]), order.Uint32(raw[28:])},
		Salt1:             binary.BigEndian.Uint32(raw[32:]),
		Salt2:             binary.BigEndian.Uint32(raw[36:]),
		Checksum:          Checksum{order.Uint32(raw[40:]), order.Uint32(raw[44:])},
	}
	h.ChecksumOK = sumWordPairs(bigEndian, raw[:indexChecksumLength], Checksum{}) == h.Checksum
	return h
}

func allZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func indexSlot(frame uint32) (block int, entryOffset int) {
	if frame <= FramesInFirstIndexBlock {
		return 0, IndexHeaderRegionSize + int(frame-1)*4
	}
	rest := frame - FramesInFirstIndexBlock - 1
	return 1 + int(rest/FramesPerIndexBlock), int(rest%FramesPerIndexBlock) * 4
}

func (x *Index) FramePage(frame uint32) (uint32, error) {
	if !x.Initialized {
		return 0, fmt.Errorf("%w: the wal-index is not initialized", ErrFrameNotIndexed)
	}
	if frame == 0 {
		return 0, fmt.Errorf("%w: frame numbers start at 1", ErrFrameNotIndexed)
	}
	block, offset := indexSlot(frame)
	position := block*IndexBlockSize + offset
	if position+4 > len(x.data) {
		return 0, fmt.Errorf("%w: frame %d maps to byte %d of a %d-byte wal-index", ErrFrameNotIndexed, frame, position, len(x.data))
	}
	return x.order.Uint32(x.data[position:]), nil
}

func (x *Index) FramePages(maxFrame uint32) ([]uint32, error) {
	var pages []uint32
	for frame := uint32(1); frame <= maxFrame; frame++ {
		page, err := x.FramePage(frame)
		if err != nil {
			return pages, err
		}
		pages = append(pages, page)
	}
	return pages, nil
}
