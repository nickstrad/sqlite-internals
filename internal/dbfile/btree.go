package dbfile

import "fmt"

type PageType uint8

const (
	IndexInterior PageType = 0x02
	TableInterior PageType = 0x05
	IndexLeaf     PageType = 0x0a
	TableLeaf     PageType = 0x0d
)

func (t PageType) Valid() bool {
	return t == IndexInterior || t == TableInterior || t == IndexLeaf || t == TableLeaf
}

func (t PageType) IsLeaf() bool  { return t == IndexLeaf || t == TableLeaf }
func (t PageType) IsTable() bool { return t == TableInterior || t == TableLeaf }

func (t PageType) HeaderSize() int {
	if t.IsLeaf() {
		return 8
	}
	return 12
}

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

type Freeblock struct {
	Offset int
	Size   int
}

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

func ParseBTreePage(data []byte, number int, usableSize int) (*BTreePage, error) {
	if usableSize < MinUsableSize || usableSize > len(data) {
		return nil, corruptPage(number, "usable size %d does not fit a %d-byte page", usableSize, len(data))
	}
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
	previousEnd := 0
	for next := p.FirstFreeblock; next != 0; {
		if len(p.Freeblocks) >= usableSize/4 {
			return nil, corruptAt(number, next, "freeblock chain does not end")
		}
		if next < p.CellContentStart || next > usableSize-4 {
			return nil, corruptAt(number, next, "freeblock at %d is outside the cell content area %d..%d", next, p.CellContentStart, usableSize-4)
		}
		if next < previousEnd {
			return nil, corruptAt(number, next, "freeblock at %d overlaps or precedes the freeblock ending at %d", next, previousEnd)
		}
		size := be16(data[next+2:])
		if size < 4 || next+size > usableSize {
			return nil, corruptAt(number, next, "freeblock at %d has size %d, outside 4..%d", next, size, usableSize-next)
		}
		p.Freeblocks = append(p.Freeblocks, Freeblock{Offset: next, Size: size})
		previousEnd = next + size
		next = be16(data[next:])
	}
	return p, nil
}

func (f *File) BTreePage(n int) (*BTreePage, error) {
	data, err := f.Page(n)
	if err != nil {
		return nil, err
	}
	return ParseBTreePage(data, n, f.UsableSize())
}

func (p *BTreePage) FreeblockBytes() int {
	total := 0
	for _, b := range p.Freeblocks {
		total += b.Size
	}
	return total
}

func (p *BTreePage) Unused() int {
	pointerEnd := p.HeaderOffset + p.Type.HeaderSize() + 2*p.CellCount
	return p.CellContentStart - pointerEnd + p.FragmentedBytes + p.FreeblockBytes()
}

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

func (c Cell) Overflows() bool { return c.PayloadSize > uint64(c.LocalSize) }

func MaxLocalPayload(usableSize int, t PageType) int {
	if t.IsTable() {
		return usableSize - 35
	}
	return (usableSize-12)*64/255 - 23
}

func MinLocalPayload(usableSize int) int {
	return (usableSize-12)*32/255 - 23
}

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

const MaxPayloadSize = 0x7fffffff

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
	if c.Size < 4 {
		c.Size = 4
	}
	return c, nil
}

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
