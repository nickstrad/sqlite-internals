package walfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
)

// On-disk wal-index (-shm) layout constants (src/wal.c:159-178, 311-474;
// src/os_unix.c:4602-4603).
const (
	// IndexHeaderSize is the byte size of one WalIndexHdr struct (src/wal.c:321-333).
	IndexHeaderSize = 48
	// IndexHeaderCopies is the number of consecutive WalIndexHdr copies at the
	// start of -shm; readers compare both copies to detect torn writes (src/wal.c:942-954).
	IndexHeaderCopies = 2
	// CheckpointInfoOffset is the byte offset of WalCkptInfo immediately after
	// the two WalIndexHdr copies (96, src/wal.c:450).
	CheckpointInfoOffset = IndexHeaderSize * IndexHeaderCopies
	// CheckpointInfoSize is the byte size of WalCkptInfo (40, src/wal.c:394-400).
	CheckpointInfoSize = 40
	// IndexHeaderRegionSize is the 136-byte header region (WALINDEX_HDR_SIZE,
	// src/wal.c:474) preceding frame 1's page-number slot in block 0.
	IndexHeaderRegionSize = CheckpointInfoOffset + CheckpointInfoSize
	// ReadMarkCount is the number of reader lock/mark slots (WAL_NREADER = 5,
	// src/wal.c:299). Slot 0 is always 0 (database-only readers); slots 1..4
	// hold WAL frame marks.
	ReadMarkCount = 5
	// ReadMarkNotUsed is the sentinel value 0xffffffff for an unoccupied read
	// mark slot (READMARK_NOT_USED, src/wal.c:401).
	ReadMarkNotUsed = 0xffffffff
	// IndexBlockSize is the 32 KiB size of each wal-index shared-memory block.
	IndexBlockSize = 32768
	// FramesPerIndexBlock is HASHTABLE_NPAGE (4096): page-number slots in every
	// index block after block 0 (src/wal.c:166).
	FramesPerIndexBlock = 4096
	// FramesInFirstIndexBlock is HASHTABLE_NPAGE_ONE (4062): page-number slots
	// in block 0 after subtracting the 136-byte header region (src/wal.c:167).
	FramesInFirstIndexBlock = FramesPerIndexBlock - IndexHeaderRegionSize/4
	// LockBytesOffset is byte offset 120 (WALINDEX_LOCK_OFFSET / UNIX_SHM_BASE),
	// where the unix VFS places fcntl byte-range locks (src/wal.c:458, 473;
	// src/os_unix.c:4602).
	LockBytesOffset = 120
	// LockByteCount is the 8 lock bytes covering WRITE, CKPT, RECOVER, and
	// READ(0..4) (src/wal.c:294-299, 458-460).
	LockByteCount = 8
	// DeadManSwitchOffset is byte offset 128 (UNIX_SHM_DMS, src/os_unix.c:4603),
	// locked by the first opener to detect whether -shm must be truncated and
	// recovered.
	DeadManSwitchOffset = LockBytesOffset + LockByteCount
	// IndexFormatVersion is WALINDEX_MAX_VERSION (3007000, src/wal.c:948).
	IndexFormatVersion  = 3007000
	indexChecksumLength = 40
)

// IndexHeader holds one decoded 48-byte WalIndexHdr copy from the -shm file
// (src/wal.c:321-333). Multi-byte fields use the host's native byte order
// except Salt1 and Salt2, which are copied as raw big-endian bytes from the WAL
// header (src/wal.c:144-146, 1457).
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

// PageSize decodes PageSizeField using SQLite's walPagesize formula
// (src/wal.c:2131-2133), mapping stored value 1 to 65,536 bytes.
func (h IndexHeader) PageSize() int {
	return int(h.PageSizeField&0xfe00) + int(h.PageSizeField&0x0001)<<16
}

// String formats the key fields of h on one line.
func (h IndexHeader) String() string {
	return fmt.Sprintf("version=%d change=%d is-init=%d page-size=%d mxFrame=%d nPage=%d salt-1=0x%08x salt-2=0x%08x checksum-ok=%v",
		h.Version, h.Change, h.IsInit, h.PageSize(), h.MaxFrame, h.Pages, h.Salt1, h.Salt2, h.ChecksumOK)
}

// CheckpointInfo holds the decoded WalCkptInfo fields at -shm bytes 96..135
// (src/wal.c:394-401): nBackfill, the five read-mark slots, and
// nBackfillAttempted. Lock bytes 120..127 are never read or written by SQLite.
type CheckpointInfo struct {
	Backfill          uint32
	ReadMarks         [ReadMarkCount]uint32
	BackfillAttempted uint32
}

// ReadMarkText formats a single read-mark slot, printing ReadMarkNotUsed
// (0xffffffff) as "unused".
func ReadMarkText(mark uint32) string {
	if mark == ReadMarkNotUsed {
		return "unused"
	}
	return fmt.Sprintf("%d", mark)
}

// ReadMarksText formats all five read-mark slots as "[0]=... [4]=...".
func (c CheckpointInfo) ReadMarksText() string {
	parts := make([]string, len(c.ReadMarks))
	for i, mark := range c.ReadMarks {
		parts[i] = fmt.Sprintf("[%d]=%s", i, ReadMarkText(mark))
	}
	return strings.Join(parts, " ")
}

// String formats nBackfill, nBackfillAttempted, and all read marks on one line.
func (c CheckpointInfo) String() string {
	return fmt.Sprintf("nBackfill=%d nBackfillAttempted=%d read-marks %s", c.Backfill, c.BackfillAttempted, c.ReadMarksText())
}

// Index is a decoded wal-index (-shm) snapshot containing both WalIndexHdr
// copies, CheckpointInfo, detected native byte order, and the backing bytes for
// page-number array lookups.
type Index struct {
	Size        int
	Initialized bool
	BigEndian   bool
	Headers     [IndexHeaderCopies]IndexHeader
	Checkpoint  CheckpointInfo
	order       binary.ByteOrder
	data        []byte
}

// Header returns copy 1 of WalIndexHdr (written second after the memory barrier
// in walIndexWriteHdr, src/wal.c:951-953).
func (x *Index) Header() IndexHeader { return x.Headers[0] }

// HeadersAgree reports whether both WalIndexHdr copies in -shm are identical.
func (x *Index) HeadersAgree() bool {
	return bytes.Equal(x.data[:IndexHeaderSize], x.data[IndexHeaderSize:IndexHeaderCopies*IndexHeaderSize])
}

// ByteOrderName returns "big-endian" or "little-endian" for the detected host
// byte order of the -shm file.
func (x *Index) ByteOrderName() string {
	if x.BigEndian {
		return "big-endian"
	}
	return "little-endian"
}

// ParseIndexFile reads a -shm file from disk and decodes it with ParseIndex.
func ParseIndexFile(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIndex(data)
}

// ParseIndex decodes a raw wal-index (-shm) byte slice. Because -shm uses the
// host's native byte order (src/wal.c:144-146), the byte order is detected from
// the version word (3007000). An all-zero first header returns Initialized=false
// with a nil error (a freshly truncated or zeroed -shm before recovery).
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
		second := data[IndexHeaderSize:]
		switch {
		case allZero(second[:IndexHeaderSize]):
			index.order = binary.LittleEndian
			return index, nil
		case binary.LittleEndian.Uint32(second) == IndexFormatVersion:
			index.order = binary.LittleEndian
		case binary.BigEndian.Uint32(second) == IndexFormatVersion:
			index.order = binary.BigEndian
			index.BigEndian = true
		default:
			return nil, fmt.Errorf("%w: first header zero, second header word %08x", ErrIndexVersion, binary.BigEndian.Uint32(second))
		}
		index.decodeHeaderRegion()
		return index, nil
	default:
		return nil, fmt.Errorf("%w: first word %08x", ErrIndexVersion, binary.BigEndian.Uint32(data))
	}
	index.Initialized = true
	index.decodeHeaderRegion()
	return index, nil
}

func (x *Index) decodeHeaderRegion() {
	data := x.data
	for i := range x.Headers {
		x.Headers[i] = decodeIndexHeader(data[i*IndexHeaderSize:(i+1)*IndexHeaderSize], x.order, x.BigEndian)
	}
	info := data[CheckpointInfoOffset:]
	x.Checkpoint.Backfill = x.order.Uint32(info[0:])
	for i := range x.Checkpoint.ReadMarks {
		x.Checkpoint.ReadMarks[i] = x.order.Uint32(info[4+4*i:])
	}
	x.Checkpoint.BackfillAttempted = x.order.Uint32(info[32:])
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
		// Salts are copied verbatim from the big-endian WAL header (src/wal.c:1457).
		Salt1:    binary.BigEndian.Uint32(raw[32:]),
		Salt2:    binary.BigEndian.Uint32(raw[36:]),
		Checksum: Checksum{order.Uint32(raw[40:]), order.Uint32(raw[44:])},
	}
	// walIndexWriteHdr checksums bytes 0..39 in native host byte order (src/wal.c:949).
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

// indexSlot maps a 1-based WAL frame number to its 32 KiB index block and byte
// offset within that block's page-number array (src/wal.c:159-177).
func indexSlot(frame uint32) (block int, entryOffset int) {
	if frame <= FramesInFirstIndexBlock {
		return 0, IndexHeaderRegionSize + int(frame-1)*4
	}
	rest := frame - FramesInFirstIndexBlock - 1
	return 1 + int(rest/FramesPerIndexBlock), int(rest%FramesPerIndexBlock) * 4
}

// FramePage returns the database page number recorded in the wal-index
// page-mapping array for 1-based frame.
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

// FramePages returns the page-mapping array entries for frames 1..maxFrame.
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
