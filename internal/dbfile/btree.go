package dbfile

import (
	"fmt"
	"sort"
)

// PageType is the 1-byte flag at offset 0 of a B-tree page header
// (decodeFlags, src/btree.c:2028-2085).
type PageType uint8

// Valid B-tree page flag bytes (src/btreeInt.h:129, src/btree.c:2025-2083).
const (
	// IndexInterior (0x02) marks an interior index B-tree page (PTF_ZERODATA).
	IndexInterior PageType = 0x02
	// TableInterior (0x05) marks an interior table B-tree page (PTF_LEAFDATA|PTF_INTKEY).
	TableInterior PageType = 0x05
	// IndexLeaf (0x0a) marks a leaf index B-tree page (PTF_ZERODATA|PTF_LEAF).
	IndexLeaf PageType = 0x0a
	// TableLeaf (0x0d) marks a leaf table B-tree page (PTF_LEAFDATA|PTF_INTKEY|PTF_LEAF).
	TableLeaf PageType = 0x0d
)

// Valid reports whether t is one of the four legal B-tree page types.
func (t PageType) Valid() bool {
	return t == IndexInterior || t == TableInterior || t == IndexLeaf || t == TableLeaf
}

// IsLeaf reports whether t is a leaf page (no child pointers).
func (t PageType) IsLeaf() bool { return t == IndexLeaf || t == TableLeaf }

// IsTable reports whether t belongs to an intkey table B-tree rather than an index B-tree.
func (t PageType) IsTable() bool { return t == TableInterior || t == TableLeaf }

// HeaderSize returns the byte length of the B-tree page header: 8 for leaves,
// 12 for interior pages (which append the 4-byte rightmost child pointer).
func (t PageType) HeaderSize() int {
	if t.IsLeaf() {
		return 8
	}
	return 12
}

// String returns the canonical role name for a B-tree page type.
func (t PageType) String() string {
	switch t {
	case IndexInterior:
		return "index-interior"
	case TableInterior:
		return "table-interior"
	case IndexLeaf:
		return "index-leaf"
	case TableLeaf:
		return "table-leaf"
	}
	return fmt.Sprintf("invalid(0x%02x)", uint8(t))
}

// Freeblock describes one linked free region (at least 4 bytes) inside a B-tree
// page's cell content area (src/btreeInt.h:152-164).
type Freeblock struct {
	Offset int
	Size   int
}

// BTreePage holds the decoded header, cell pointer array, freeblock chain, and
// raw bytes of one B-tree page (btreeInitPage, src/btree.c:2218-2265).
type BTreePage struct {
	Number           int
	Type             PageType
	HeaderOffset     int
	FirstFreeblock   int
	CellCount        int
	CellContentStart int
	FragmentedBytes  int
	RightChild       uint32
	CellPointers     []int
	Freeblocks       []Freeblock
	Data             []byte
	UsableSize       int
}

func headerOffsetFor(number int) int {
	if number == 1 {
		return HeaderSize
	}
	return 0
}

// ParseBTreePage decodes and validates a B-tree page header, its cell pointer
// array, and its freeblock chain from raw page bytes.
func ParseBTreePage(data []byte, number int, usableSize int) (*BTreePage, error) {
	if usableSize < MinUsableSize || usableSize > len(data) {
		return nil, corruptPage(number, "usable size %d does not fit a %d-byte page", usableSize, len(data))
	}
	// Page 1's B-tree header starts after the 100-byte database file header.
	offset := headerOffsetFor(number)
	if offset+8 > usableSize {
		return nil, corruptAt(number, offset, "page too small for a b-tree page header")
	}
	p := &BTreePage{Number: number, HeaderOffset: offset, Data: data, UsableSize: usableSize}
	p.Type = PageType(data[offset])
	if !p.Type.Valid() {
		return nil, corruptAt(number, offset, "page type byte 0x%02x is not 0x02, 0x05, 0x0a or 0x0d", data[offset])
	}
	p.FirstFreeblock = be16(data[offset+1:])
	p.CellCount = be16(data[offset+3:])
	p.CellContentStart = be16(data[offset+5:])
	// On a 65536-byte page with no cells, 65536 wraps to 0 in the 2-byte field (get2byteNotZero, src/btree.c:2257).
	if p.CellContentStart == 0 {
		p.CellContentStart = 65536
	}
	p.FragmentedBytes = int(data[offset+7])
	if !p.Type.IsLeaf() {
		p.RightChild = be32(data[offset+8:])
	}
	pointerStart := offset + p.Type.HeaderSize()
	pointerEnd := pointerStart + 2*p.CellCount
	if pointerEnd > usableSize {
		return nil, corruptAt(number, offset+3, "%d cells do not fit: the cell pointer array would end at %d, usable size %d", p.CellCount, pointerEnd, usableSize)
	}
	if p.CellContentStart < pointerEnd || p.CellContentStart > usableSize {
		return nil, corruptAt(number, offset+5, "cell content area starts at %d, outside %d..%d", p.CellContentStart, pointerEnd, usableSize)
	}
	p.CellPointers = make([]int, p.CellCount)
	// Every cell occupies at least 4 bytes (leaves) or 5 bytes (interior: 4-byte child + 1-byte varint).
	lastCellStart := usableSize - 4
	if !p.Type.IsLeaf() {
		lastCellStart--
	}
	for i := range p.CellCount {
		cellOffset := be16(data[pointerStart+2*i:])
		if cellOffset < p.CellContentStart || cellOffset > lastCellStart {
			return nil, corruptAt(number, pointerStart+2*i, "cell %d pointer %d is outside the cell content area %d..%d", i, cellOffset, p.CellContentStart, lastCellStart)
		}
		p.CellPointers[i] = cellOffset
	}
	// Walk the freeblock list verifying ascending non-overlapping offsets and minimum 4-byte size (src/btreeInt.h:152-164).
	previousEnd := 0
	for next := p.FirstFreeblock; next != 0; {
		if len(p.Freeblocks) >= usableSize/4 {
			return nil, corruptAt(number, next, "freeblock chain does not end")
		}
		if next < p.CellContentStart || next > usableSize-4 {
			return nil, corruptAt(number, next, "freeblock at %d is outside the cell content area %d..%d", next, p.CellContentStart, usableSize-4)
		}
		if previousEnd > 0 && next < previousEnd+4 {
			return nil, corruptAt(number, next, "freeblock at %d is less than 4 bytes after the freeblock ending at %d", next, previousEnd)
		}
		size := be16(data[next+2:])
		if size < 4 || next+size > usableSize {
			return nil, corruptAt(number, next, "freeblock at %d has size %d, outside 4..%d", next, size, usableSize-next)
		}
		p.Freeblocks = append(p.Freeblocks, Freeblock{Offset: next, Size: size})
		previousEnd = next + size
		next = be16(data[next:])
	}
	if free := p.CellContentStart + p.FragmentedBytes + p.FreeblockBytes(); free > usableSize {
		return nil, corruptAt(number, offset+7, "content start %d + fragmented bytes %d + freeblock bytes %d = %d exceeds the usable size %d", p.CellContentStart, p.FragmentedBytes, p.FreeblockBytes(), free, usableSize)
	}
	if err := p.checkCoverage(); err != nil {
		return nil, err
	}
	return p, nil
}

type byteRange struct{ first, last int }

func (p *BTreePage) checkCoverage() error {
	ranges := make([]byteRange, 0, p.CellCount+len(p.Freeblocks))
	for i := range p.CellCount {
		c, err := p.Cell(i)
		if err != nil {
			return err
		}
		ranges = append(ranges, byteRange{c.Offset, c.Offset + c.Size - 1})
	}
	for _, b := range p.Freeblocks {
		ranges = append(ranges, byteRange{b.Offset, b.Offset + b.Size - 1})
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].first != ranges[j].first {
			return ranges[i].first < ranges[j].first
		}
		return ranges[i].last < ranges[j].last
	})
	fragmented := 0
	previousLast := p.CellContentStart - 1
	for _, r := range ranges {
		if r.first <= previousLast {
			return corruptAt(p.Number, r.first, "byte %d is used twice (by two cells, or a cell and a freeblock)", r.first)
		}
		fragmented += r.first - previousLast - 1
		previousLast = r.last
	}
	fragmented += p.UsableSize - previousLast - 1
	if fragmented != p.FragmentedBytes {
		return corruptAt(p.Number, p.HeaderOffset+7, "%d bytes of the cell content area belong to no cell or freeblock, the header reports %d fragmented bytes", fragmented, p.FragmentedBytes)
	}
	return nil
}

// BTreePage reads page n from f and parses it as a B-tree page.
func (f *File) BTreePage(n int) (*BTreePage, error) {
	data, err := f.Page(n)
	if err != nil {
		return nil, err
	}
	return ParseBTreePage(data, n, f.UsableSize())
}

// FreeblockBytes returns the sum of the byte sizes of all freeblocks on p.
func (p *BTreePage) FreeblockBytes() int {
	total := 0
	for _, b := range p.Freeblocks {
		total += b.Size
	}
	return total
}

// Unused returns the total free bytes on p (unallocated gap + fragmented bytes
// + freeblocks), matching dbstat's unused column.
func (p *BTreePage) Unused() int {
	pointerEnd := p.HeaderOffset + p.Type.HeaderSize() + 2*p.CellCount
	return p.CellContentStart - pointerEnd + p.FragmentedBytes + p.FreeblockBytes()
}

// Cell holds one parsed B-tree cell: its optional left-child pointer, payload
// size, rowid, local payload slice, and optional first overflow page number.
type Cell struct {
	Index         int
	Offset        int
	LeftChild     uint32
	PayloadSize   uint64
	Rowid         int64
	HasRowid      bool
	LocalSize     int
	Local         []byte
	FirstOverflow uint32
	Size          int
}

// Overflows reports whether part of the cell's payload spills onto overflow pages.
func (c Cell) Overflows() bool { return c.PayloadSize > uint64(c.LocalSize) }

// MaxLocalPayload returns the maximum payload bytes stored locally in one cell
// before spilling to overflow pages (lockBtree, src/btree.c:3441-3444).
func MaxLocalPayload(usableSize int, t PageType) int {
	if t.IsTable() {
		return usableSize - 35
	}
	return (usableSize-12)*64/255 - 23
}

// MinLocalPayload returns the minimum payload bytes retained locally when a
// cell overflows (lockBtree, src/btree.c:3442, 3444).
func MinLocalPayload(usableSize int) int {
	return (usableSize-12)*32/255 - 23
}

// LocalPayloadSize computes how many bytes of a payload of size payloadSize are
// stored on the B-tree page itself rather than in overflow pages. Matches
// btreeParseCellAdjustSizeForOverflow (src/btree.c:1178-1207), which maximizes
// occupancy of the last overflow page.
func LocalPayloadSize(usableSize int, t PageType, payloadSize uint64) int {
	maxLocal := MaxLocalPayload(usableSize, t)
	if payloadSize <= uint64(maxLocal) {
		return int(payloadSize)
	}
	minLocal := MinLocalPayload(usableSize)
	surplus := uint64(minLocal) + (payloadSize-uint64(minLocal))%uint64(usableSize-4)
	if surplus <= uint64(maxLocal) {
		return int(surplus)
	}
	return minLocal
}

// MaxPayloadSize is the largest legal cell payload size (2^31 - 1 bytes).
const MaxPayloadSize = 0x7fffffff

// Cell parses the i-th cell on p according to p.Type, mirroring
// btreeParseCellPtr, btreeParseCellPtrIndex, and btreeParseCellPtrNoPayload
// (src/btree.c:1242-1385).
func (p *BTreePage) Cell(i int) (Cell, error) {
	if i < 0 || i >= p.CellCount {
		return Cell{}, fmt.Errorf("page %d has %d cells, no cell %d", p.Number, p.CellCount, i)
	}
	offset := p.CellPointers[i]
	c := Cell{Index: i, Offset: offset}
	limit := p.Data[:p.UsableSize]
	pos := offset
	readVarint := func(what string) (uint64, error) {
		value, n, err := Varint(limit[pos:])
		if err != nil {
			return 0, corruptAt(p.Number, pos, "cell %d %s: %v", i, what, err)
		}
		pos += n
		return value, nil
	}
	if !p.Type.IsLeaf() {
		if pos+4 > len(limit) {
			return c, corruptAt(p.Number, pos, "cell %d left child pointer runs past the usable size", i)
		}
		c.LeftChild = be32(limit[pos:])
		pos += 4
	}
	if p.Type == TableInterior {
		key, err := readVarint("rowid")
		if err != nil {
			return c, err
		}
		c.Rowid, c.HasRowid = int64(key), true
		c.Size = pos - offset
		return c, nil
	}
	payloadSize, err := readVarint("payload size")
	if err != nil {
		return c, err
	}
	if payloadSize > MaxPayloadSize {
		return c, corruptAt(p.Number, offset, "cell %d payload size %d is larger than %d", i, payloadSize, MaxPayloadSize)
	}
	c.PayloadSize = payloadSize
	if p.Type == TableLeaf {
		key, err := readVarint("rowid")
		if err != nil {
			return c, err
		}
		c.Rowid, c.HasRowid = int64(key), true
	}
	c.LocalSize = LocalPayloadSize(p.UsableSize, p.Type, payloadSize)
	end := pos + c.LocalSize
	if c.Overflows() {
		end += 4
	}
	if end > len(limit) {
		return c, corruptAt(p.Number, offset, "cell %d ends at %d, past the usable size %d", i, end, len(limit))
	}
	c.Local = limit[pos : pos+c.LocalSize]
	if c.Overflows() {
		c.FirstOverflow = be32(limit[pos+c.LocalSize:])
		if c.FirstOverflow == 0 {
			return c, corruptAt(p.Number, pos+c.LocalSize, "cell %d overflows but its first overflow page is 0", i)
		}
	}
	c.Size = end - offset
	// SQLite enforces a 4-byte minimum cell size so any freed cell can become a
	// 4-byte freeblock header (src/btree.c:1341, 1380).
	if c.Size < 4 {
		c.Size = 4
	}
	return c, nil
}

// Cells parses and returns all cells on p in cell-pointer (key) order.
func (p *BTreePage) Cells() ([]Cell, error) {
	cells := make([]Cell, 0, p.CellCount)
	for i := range p.CellCount {
		c, err := p.Cell(i)
		if err != nil {
			return nil, err
		}
		cells = append(cells, c)
	}
	return cells, nil
}
