package journalfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

var Magic = [8]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

const (
	HeaderFieldsSize   = 28
	NoSyncRecordCount  = 0xffffffff
	RecordOverhead     = 8
	pendingByte        = 0x40000000
	minSectorSize      = 32
	maxSectorSize      = 0x10000
	minPageSize        = 512
	maxPageSize        = 65536
	superJournalFooter = 16
)

type HeaderState int

const (
	HeaderSynced HeaderState = iota
	HeaderNoSync
	HeaderNotYetSynced
	HeaderZeroed
	HeaderInvalid
	HeaderMissing
)

func (s HeaderState) String() string {
	switch s {
	case HeaderSynced:
		return "synced"
	case HeaderNoSync:
		return "no-sync"
	case HeaderNotYetSynced:
		return "not-yet-synced"
	case HeaderZeroed:
		return "zeroed"
	case HeaderInvalid:
		return "invalid"
	case HeaderMissing:
		return "missing"
	}
	return fmt.Sprintf("HeaderState(%d)", int(s))
}

type Header struct {
	Offset       int64
	State        HeaderState
	MagicOK      bool
	RecordCount  uint32
	Nonce        uint32
	InitialPages uint32
	SectorSize   uint32
	PageSize     uint32
}

func (h Header) CountsRecords() bool {
	return h.MagicOK && h.RecordCount != NoSyncRecordCount
}

type Record struct {
	Offset           int64
	Page             uint32
	StoredChecksum   uint32
	ComputedChecksum uint32
	Image            []byte
}

func (r Record) ChecksumOK() bool {
	return r.StoredChecksum == r.ComputedChecksum
}

type Segment struct {
	Header  Header
	Records []Record
}

type Journal struct {
	Size         int64
	FirstByte    byte
	Segments     []Segment
	SuperJournal string
	TrailingByte int64
}

type PlaybackOutcome struct {
	Hot          bool
	TruncateTo   uint32
	Pages        []uint32
	SkippedPages []uint32
	StopReason   string
}

func ReadFile(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Parse(data []byte) (*Journal, error) {
	j := &Journal{Size: int64(len(data))}
	if len(data) > 0 {
		j.FirstByte = data[0]
	}
	end := int64(len(data))
	if name, start, ok := superJournalName(data); ok {
		j.SuperJournal = name
		end = start
	}
	if end < HeaderFieldsSize {
		if end > 0 {
			j.Segments = append(j.Segments, Segment{Header: Header{State: HeaderMissing}})
			j.TrailingByte = end
		}
		return j, nil
	}
	first := parseHeader(data, 0)
	if first.State == HeaderZeroed || first.State == HeaderInvalid {
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	if !validGeometry(first.SectorSize, first.PageSize) {
		first.State = HeaderInvalid
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	sector := int64(first.SectorSize)
	pageSize := int64(first.PageSize)
	recordSize := pageSize + RecordOverhead
	offset := int64(0)
	header := first
	for {
		segment := Segment{Header: header}
		position := offset + sector
		limit := int64(-1)
		if header.CountsRecords() {
			limit = int64(header.RecordCount)
		}
		for (limit < 0 || int64(len(segment.Records)) < limit) && position+recordSize <= end {
			segment.Records = append(segment.Records, parseRecord(data, position, pageSize, header.Nonce))
			position += recordSize
		}
		j.Segments = append(j.Segments, segment)
		if limit < 0 || int64(len(segment.Records)) < limit {
			j.TrailingByte = end - position
			break
		}
		next := roundUp(position, sector)
		if next+sector > end {
			j.TrailingByte = end - position
			break
		}
		header = parseHeader(data, next)
		if header.State != HeaderSynced && header.State != HeaderNotYetSynced && header.State != HeaderNoSync {
			j.TrailingByte = end - position
			break
		}
		header.SectorSize = first.SectorSize
		header.PageSize = first.PageSize
		offset = next
	}
	return j, nil
}

func parseHeader(data []byte, offset int64) Header {
	h := Header{Offset: offset}
	fields := data[offset : offset+HeaderFieldsSize]
	h.MagicOK = bytes.Equal(fields[:8], Magic[:])
	h.RecordCount = binary.BigEndian.Uint32(fields[8:12])
	h.Nonce = binary.BigEndian.Uint32(fields[12:16])
	h.InitialPages = binary.BigEndian.Uint32(fields[16:20])
	h.SectorSize = binary.BigEndian.Uint32(fields[20:24])
	h.PageSize = binary.BigEndian.Uint32(fields[24:28])
	switch {
	case allZero(fields):
		h.State = HeaderZeroed
	case h.MagicOK && h.RecordCount == NoSyncRecordCount:
		h.State = HeaderNoSync
	case h.MagicOK:
		h.State = HeaderSynced
	case allZero(fields[:12]):
		h.State = HeaderNotYetSynced
	default:
		h.State = HeaderInvalid
	}
	return h
}

func parseRecord(data []byte, offset, pageSize int64, nonce uint32) Record {
	image := data[offset+4 : offset+4+pageSize]
	return Record{
		Offset:           offset,
		Page:             binary.BigEndian.Uint32(data[offset : offset+4]),
		StoredChecksum:   binary.BigEndian.Uint32(data[offset+4+pageSize : offset+8+pageSize]),
		ComputedChecksum: Checksum(nonce, image),
		Image:            image,
	}
}

func Checksum(nonce uint32, image []byte) uint32 {
	sum := nonce
	for i := len(image) - 200; i > 0; i -= 200 {
		sum += uint32(image[i])
	}
	return sum
}

func SuperJournalPage(pageSize uint32) uint32 {
	if pageSize == 0 {
		return 0
	}
	return pendingByte/pageSize + 1
}

func superJournalName(data []byte) (string, int64, bool) {
	size := int64(len(data))
	if size < superJournalFooter+4 || !bytes.Equal(data[size-8:], Magic[:]) {
		return "", 0, false
	}
	length := int64(binary.BigEndian.Uint32(data[size-16 : size-12]))
	stored := binary.BigEndian.Uint32(data[size-12 : size-8])
	nameStart := size - superJournalFooter - length
	if length <= 0 || nameStart < 4 {
		return "", 0, false
	}
	name := data[nameStart : size-superJournalFooter]
	var sum uint32
	for _, b := range name {
		sum += uint32(b)
	}
	if sum != stored {
		return "", 0, false
	}
	return string(bytes.TrimRight(name, "\x00")), nameStart - 4, true
}

func validGeometry(sector, pageSize uint32) bool {
	powerOfTwo := func(v uint32) bool { return v != 0 && v&(v-1) == 0 }
	return sector >= minSectorSize && sector <= maxSectorSize && powerOfTwo(sector) &&
		pageSize >= minPageSize && pageSize <= maxPageSize && powerOfTwo(pageSize)
}

func roundUp(offset, unit int64) int64 {
	if offset == 0 {
		return 0
	}
	return ((offset-1)/unit + 1) * unit
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func (j *Journal) LooksHot() bool {
	return j.Size > 0 && j.FirstByte != 0
}

func (j *Journal) RecordsPresent() int {
	n := 0
	for _, s := range j.Segments {
		n += len(s.Records)
	}
	return n
}

func (j *Journal) ChecksumFailures() int {
	n := 0
	for _, s := range j.Segments {
		for _, r := range s.Records {
			if !r.ChecksumOK() {
				n++
			}
		}
	}
	return n
}

func (j *Journal) Playback() PlaybackOutcome {
	outcome := PlaybackOutcome{Hot: j.LooksHot()}
	if !outcome.Hot {
		outcome.StopReason = "not hot: empty file or first byte is zero"
		return outcome
	}
	if len(j.Segments) == 0 {
		outcome.StopReason = "no header"
		return outcome
	}
	first := j.Segments[0].Header
	if first.State != HeaderSynced && first.State != HeaderNoSync {
		outcome.StopReason = "first header is " + first.State.String()
		return outcome
	}
	outcome.TruncateTo = first.InitialPages
	superPage := SuperJournalPage(first.PageSize)
	for _, segment := range j.Segments {
		h := segment.Header
		if !h.MagicOK {
			outcome.StopReason = fmt.Sprintf("header at offset %d has no magic", h.Offset)
			return outcome
		}
		count := len(segment.Records)
		if h.RecordCount != NoSyncRecordCount {
			if int64(h.RecordCount) > int64(count) {
				outcome.StopReason = fmt.Sprintf("header at offset %d counts %d records but only %d are complete", h.Offset, h.RecordCount, count)
				for _, r := range segment.Records {
					if stop := outcome.take(r, first.InitialPages, superPage); stop != "" {
						outcome.StopReason = stop
						return outcome
					}
				}
				return outcome
			}
			count = int(h.RecordCount)
		}
		for _, r := range segment.Records[:count] {
			if stop := outcome.take(r, first.InitialPages, superPage); stop != "" {
				outcome.StopReason = stop
				return outcome
			}
		}
	}
	outcome.StopReason = "end of journal"
	return outcome
}

func (o *PlaybackOutcome) take(r Record, initialPages, superPage uint32) string {
	if r.Page == 0 || r.Page == superPage {
		return fmt.Sprintf("record at offset %d has page number %d", r.Offset, r.Page)
	}
	if r.Page > initialPages {
		o.SkippedPages = append(o.SkippedPages, r.Page)
		return ""
	}
	if !r.ChecksumOK() {
		return fmt.Sprintf("record at offset %d (page %d) fails its checksum", r.Offset, r.Page)
	}
	o.Pages = append(o.Pages, r.Page)
	return ""
}
