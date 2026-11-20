// Package journalfile implements a read-only inspector for SQLite rollback
// journal (-journal) files, using plain Go with no SQLite dependency.
//
// It decodes journal headers, page records, checksums, and super-journal
// trailers, and predicts which pages SQLite would restore during hot-journal
// playback. It never writes to a database or replays a journal itself.
package journalfile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// Magic is the 8-byte signature that identifies a live rollback journal header
// (aJournalMagic in src/pager.c:757).
var Magic = [8]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}

// Journal format sizes and bounds matching src/pager.c:1449-1693.
const (
	// HeaderFieldsSize is the byte length of the six big-endian fields at the
	// start of a journal header (8-byte magic + five 4-byte integers).
	HeaderFieldsSize = 28
	// NoSyncRecordCount (0xffffffff) in a header's record-count field tells
	// SQLite to infer the record count from the file size (src/pager.c:1499).
	NoSyncRecordCount = 0xffffffff
	// RecordOverhead is the 8 bytes wrapping each journaled page image:
	// 4-byte page number before the image and 4-byte checksum after it
	// (pagerAddPageToRollbackJournal, src/pager.c:6062-6067).
	RecordOverhead     = 8
	pendingByte        = 0x40000000
	minSectorSize      = 32
	maxSectorSize      = 0x10000
	minPageSize        = 512
	maxPageSize        = 65536
	superJournalFooter = 16
	maxSuperJournalLen = 512
)

// HeaderState classifies the 28-byte header at the start of a journal segment.
type HeaderState int

// Rollback journal header states recognized by parseHeader.
const (
	// HeaderSynced indicates a header carrying Magic and an explicit record
	// count, written by syncJournal (src/pager.c:4377-4410).
	HeaderSynced HeaderState = iota
	// HeaderNoSync indicates a header carrying Magic and NoSyncRecordCount,
	// written directly by writeJournalHdr when synchronous=OFF,
	// journal_mode=MEMORY, or SAFE_APPEND is active (src/pager.c:1495-1499).
	HeaderNoSync
	// HeaderNotYetSynced indicates a header whose first 12 bytes (magic and
	// record count) are still zero while the remaining fields are populated,
	// written by writeJournalHdr before syncJournal runs (src/pager.c:1500-1502).
	HeaderNotYetSynced
	// HeaderZeroed indicates all 28 header bytes are zero, as left after commit
	// in PERSIST mode by zeroJournalHdr (src/pager.c:1399-1432).
	HeaderZeroed
	// HeaderInvalid indicates bytes that are neither valid magic, nor zeroed,
	// nor a valid sector/page-size geometry (readJournalHdr, src/pager.c:1650-1672).
	HeaderInvalid
	// HeaderMissing indicates fewer than HeaderFieldsSize bytes were present.
	HeaderMissing
)

// String returns a short human-readable name for the header state.
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

// Header holds the decoded fields of one sector-aligned rollback journal header
// (writeJournalHdr and readJournalHdr, src/pager.c:1449-1693).
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

// CountsRecords reports whether the header carries a valid magic and an
// explicit record count rather than the NoSyncRecordCount sentinel.
func (h Header) CountsRecords() bool {
	return h.MagicOK && h.RecordCount != NoSyncRecordCount
}

// Record holds one journaled page record: the 4-byte page number, the original
// page image, the stored 4-byte checksum, and the checksum recomputed from the
// segment's nonce and page bytes (src/pager.c:6037-6085).
type Record struct {
	Offset           int64
	Page             uint32
	StoredChecksum   uint32
	ComputedChecksum uint32
	Image            []byte
}

// ChecksumOK reports whether the record's stored checksum matches the checksum
// computed by Checksum.
func (r Record) ChecksumOK() bool {
	return r.StoredChecksum == r.ComputedChecksum
}

// Segment pairs one journal header with the complete page records that follow
// it. A transaction that spills dirty pages mid-transaction writes multiple
// segments, each starting on a sector boundary (syncJournal, src/pager.c:4423-4427).
type Segment struct {
	Header  Header
	Records []Record
}

// Journal is the decoded structure of a rollback journal file.
type Journal struct {
	Size         int64
	FirstByte    byte
	Segments     []Segment
	SuperJournal string
	TrailingByte int64
}

// PlaybackOutcome predicts what SQLite's pager_playback (src/pager.c:2864-3052)
// will do if it reopens the database beside this journal.
type PlaybackOutcome struct {
	Hot          bool
	TruncateTo   uint32
	Pages        []uint32
	SkippedPages []uint32
	StopReason   string
}

// ReadFile reads path from disk and parses it as a rollback journal.
func ReadFile(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes raw rollback-journal bytes into segments, records, and any
// trailing super-journal name without modifying the input slice.
func Parse(data []byte) (*Journal, error) {
	j := &Journal{Size: int64(len(data))}
	if len(data) > 0 {
		j.FirstByte = data[0]
	}
	end := int64(len(data))
	// Check for a multi-database super-journal footer at EOF first so its bytes
	// are excluded from record parsing (pager_playback, src/pager.c:2891).
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
	// Geometry (sector size and page size) comes only from the first header;
	// SQLite stops playback if either is out of range or not a power of two
	// (readJournalHdr, src/pager.c:1650-1672).
	if !validGeometry(first.SectorSize, first.PageSize) {
		first.State = HeaderInvalid
		j.Segments = append(j.Segments, Segment{Header: first})
		return j, nil
	}
	sector := int64(first.SectorSize)
	if end < sector {
		first.State = HeaderMissing
		j.Segments = append(j.Segments, Segment{Header: first})
		j.TrailingByte = end
		return j, nil
	}
	pageSize := int64(first.PageSize)
	recordSize := pageSize + RecordOverhead
	offset := int64(0)
	header := first
	for {
		segment := Segment{Header: header}
		// Records begin one sector after the header (writeJournalHdr pads the
		// header out to a full sector, src/pager.c:1563-1568).
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
		// A subsequent segment header starts on the next sector boundary
		// (journalHdrOffset, src/pager.c:1366-1376).
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

// Checksum computes SQLite's rollback-journal page checksum: starting from
// nonce, it adds every 200th byte stepping down from len(image)-200 while the
// index is strictly positive (pager_cksum, src/pager.c:2251-2259).
func Checksum(nonce uint32, image []byte) uint32 {
	sum := nonce
	for i := len(image) - 200; i > 0; i -= 200 {
		sum += uint32(image[i])
	}
	return sum
}

// SuperJournalPage returns the reserved page number (PAGER_SJ_PGNO) that marks
// the start of a super-journal name record in the journal (the lock-byte page
// at byte offset 0x40000000, which can never be a real database page).
func SuperJournalPage(pageSize uint32) uint32 {
	if pageSize == 0 {
		return 0
	}
	return pendingByte/pageSize + 1
}

// superJournalName detects the optional multi-database super-journal footer
// written by writeSuperJournal: [4-byte PAGER_SJ_PGNO][name][4-byte len][4-byte sum][8-byte Magic].
func superJournalName(data []byte) (string, int64, bool) {
	size := int64(len(data))
	if size < superJournalFooter+4 || !bytes.Equal(data[size-8:], Magic[:]) {
		return "", 0, false
	}
	length := int64(binary.BigEndian.Uint32(data[size-16 : size-12]))
	stored := binary.BigEndian.Uint32(data[size-12 : size-8])
	nameStart := size - superJournalFooter - length
	if length <= 0 || length > maxSuperJournalLen || nameStart < 4 {
		return "", 0, false
	}
	name := data[nameStart : size-superJournalFooter]
	var sum uint32
	for _, b := range name {
		sum += uint32(int8(b))
	}
	if sum != stored || name[0] == 0 {
		return "", 0, false
	}
	if nul := bytes.IndexByte(name, 0); nul >= 0 {
		name = name[:nul]
	}
	return string(name), nameStart - 4, true
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

// LooksHot reports the byte-level half of hasHotJournal (src/pager.c:5244-5251):
// a journal is hot only if it is non-empty and its first byte is non-zero.
// The lock and database-size checks in hasHotJournal depend on external state.
func (j *Journal) LooksHot() bool {
	return j.Size > 0 && j.FirstByte != 0
}

// RecordsPresent returns the total count of complete page records across all
// parsed segments.
func (j *Journal) RecordsPresent() int {
	n := 0
	for _, s := range j.Segments {
		n += len(s.Records)
	}
	return n
}

// ChecksumFailures returns the number of complete records whose stored checksum
// disagrees with Checksum.
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

// Playback simulates the read loop of pager_playback (src/pager.c:2864-3052)
// and pager_playback_one_page (src/pager.c:2298-2500) to predict which pages
// SQLite will restore and what page count it will truncate the database to.
func (j *Journal) Playback() PlaybackOutcome {
	outcome := PlaybackOutcome{Hot: j.LooksHot()}
	if outcome.Hot && j.SuperJournal != "" {
		outcome.StopReason = fmt.Sprintf("names super-journal %q: nothing is played back unless that file exists (see PlaybackIfSuperJournalExists)", j.SuperJournal)
		return outcome
	}
	return j.playback(outcome)
}

func (j *Journal) PlaybackIfSuperJournalExists() PlaybackOutcome {
	return j.playback(PlaybackOutcome{Hot: j.LooksHot()})
}

func (j *Journal) playback(outcome PlaybackOutcome) PlaybackOutcome {
	if !outcome.Hot {
		outcome.StopReason = "not hot: empty file or first byte is zero"
		return outcome
	}
	if len(j.Segments) == 0 {
		outcome.StopReason = "no header"
		return outcome
	}
	first := j.Segments[0].Header
	// readJournalHdr returns SQLITE_DONE when the magic bytes do not match.
	if first.State != HeaderSynced && first.State != HeaderNoSync {
		outcome.StopReason = "first header is " + first.State.String()
		return outcome
	}
	// pager_playback truncates the database to the first header's dbOrigSize
	// before restoring pages (src/pager.c:2952).
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

// take applies pager_playback_one_page's per-record checks in SQLite's order
// (src/pager.c:2298-2500): page 0 or superPage stops playback, pages beyond
// initialPages are skipped, and a bad checksum stops playback.
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
