package dbfile

import "fmt"

// OverflowPage represents one page in a cell's singly linked overflow chain
// (src/btreeInt.h:198-205, getOverflowPage src/btree.c:5007-5062).
type OverflowPage struct {
	Number   int
	Next     uint32
	Content  []byte
	Payload  int
	IsLast   bool
	OwnerRow int64
}

func (f *File) checkOverflowTarget(from, target int) error {
	if target < 1 || target > f.pageCount {
		return corruptPage(from, "overflow pointer to page %d is outside 1..%d", target, f.pageCount)
	}
	if target == 1 || target == f.LockBytePage() || f.IsPointerMapPage(target) {
		return corruptPage(from, "overflow pointer to page %d, which can never be an overflow page", target)
	}
	return nil
}

// OverflowChain follows and validates all overflow pages referenced by cell c
// on sourcePage. Each overflow page begins with a 4-byte next-page number
// followed by up to usableSize-4 payload bytes (src/btree.c:5051).
func (f *File) OverflowChain(sourcePage int, c Cell) ([]OverflowPage, error) {
	if !c.Overflows() {
		return nil, nil
	}
	perPage := f.UsableSize() - 4
	remaining := c.PayloadSize - uint64(c.LocalSize)
	count := int((remaining + uint64(perPage) - 1) / uint64(perPage))
	if count > f.pageCount {
		return nil, corruptPage(sourcePage, "cell %d needs %d overflow pages, the file has %d pages", c.Index, count, f.pageCount)
	}
	chain := make([]OverflowPage, 0, count)
	seen := make(map[int]bool, count)
	from := sourcePage
	next := int(c.FirstOverflow)
	for i := range count {
		if err := f.checkOverflowTarget(from, next); err != nil {
			return nil, err
		}
		if seen[next] {
			return nil, corruptPage(from, "overflow chain of cell %d loops back to page %d", c.Index, next)
		}
		seen[next] = true
		data, err := f.Page(next)
		if err != nil {
			return nil, err
		}
		take := perPage
		if uint64(take) > remaining {
			take = int(remaining)
		}
		page := OverflowPage{Number: next, Next: be32(data[0:4]), Content: data[4 : 4+take], Payload: take, IsLast: i == count-1}
		chain = append(chain, page)
		remaining -= uint64(take)
		from = next
		next = int(page.Next)
		if i < count-1 && next == 0 {
			return nil, corruptPage(from, "overflow chain of cell %d ends after %d of %d pages", c.Index, i+1, count)
		}
		// The final overflow page terminates the list with next == 0; checkList
		// (src/btree.c:10729-10783) rejects a non-zero trailing pointer.
		if i == count-1 && next != 0 {
			return nil, corruptAt(from, 0, "last overflow page of cell %d on page %d has non-zero next pointer %d", c.Index, sourcePage, next)
		}
	}
	return chain, nil
}

// Payload assembles the complete payload bytes of cell c on sourcePage by
// concatenating its local slice with its overflow chain.
func (f *File) Payload(sourcePage int, c Cell) ([]byte, []OverflowPage, error) {
	// Guard against a corrupt payload-size varint requesting gigabytes of memory.
	if c.PayloadSize > uint64(f.size) {
		return nil, nil, corruptPage(sourcePage, "cell %d payload of %d bytes is larger than the file", c.Index, c.PayloadSize)
	}
	chain, err := f.OverflowChain(sourcePage, c)
	if err != nil {
		return nil, nil, err
	}
	payload := make([]byte, 0, c.PayloadSize)
	payload = append(payload, c.Local...)
	for _, page := range chain {
		payload = append(payload, page.Content...)
	}
	if uint64(len(payload)) != c.PayloadSize {
		return nil, nil, fmt.Errorf("page %d cell %d: assembled %d payload bytes, want %d", sourcePage, c.Index, len(payload), c.PayloadSize)
	}
	return payload, chain, nil
}
