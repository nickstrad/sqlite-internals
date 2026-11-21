package journalfile

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/walfleet/internal/sqlitex"
)

const testPageSize = 1024
const testSector = 512

type builder struct {
	data []byte
}

func (b *builder) header(offset int, magic bool, nRec, nonce, initial uint32) {
	b.grow(offset + testSector)
	if magic {
		copy(b.data[offset:], Magic[:])
	}
	binary.BigEndian.PutUint32(b.data[offset+8:], nRec)
	binary.BigEndian.PutUint32(b.data[offset+12:], nonce)
	binary.BigEndian.PutUint32(b.data[offset+16:], initial)
	binary.BigEndian.PutUint32(b.data[offset+20:], testSector)
	binary.BigEndian.PutUint32(b.data[offset+24:], testPageSize)
}

func (b *builder) record(page uint32, fill byte, nonce uint32, corrupt bool) {
	image := bytes.Repeat([]byte{fill}, testPageSize)
	rec := make([]byte, 4, testPageSize+8)
	binary.BigEndian.PutUint32(rec, page)
	rec = append(rec, image...)
	sum := Checksum(nonce, image)
	if corrupt {
		sum++
	}
	rec = binary.BigEndian.AppendUint32(rec, sum)
	b.data = append(b.data, rec...)
}

func (b *builder) grow(n int) {
	if len(b.data) < n {
		b.data = append(b.data, make([]byte, n-len(b.data))...)
	}
}

// TestChecksumSamplesEvery200thByteFromTheEnd verifies Checksum matches pager_cksum (src/pager.c:2251-2259) and skips byte 0.
func TestChecksumSamplesEvery200thByteFromTheEnd(t *testing.T) {
	image := make([]byte, 4096)
	for i := range image {
		image[i] = byte(i % 251)
	}
	var want uint32 = 7
	for i := 4096 - 200; i > 0; i -= 200 {
		want += uint32(image[i])
	}
	if got := Checksum(7, image); got != want {
		t.Fatalf("checksum %d want %d", got, want)
	}
	image[0] = 99
	if Checksum(7, image) != want {
		t.Fatal("byte 0 must not take part in the checksum")
	}
	image[4096-200]++
	if Checksum(7, image) == want {
		t.Fatal("byte pageSize-200 must take part in the checksum")
	}
}

// TestSyncedJournal verifies parsing and playback prediction for a synced hot journal with an explicit record count.
func TestSyncedJournal(t *testing.T) {
	b := &builder{}
	b.header(0, true, 2, 1234, 5)
	b.record(3, 0xaa, 1234, false)
	b.record(1, 0xbb, 1234, false)
	j, err := Parse(b.data)
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Segments) != 1 || j.RecordsPresent() != 2 || j.ChecksumFailures() != 0 {
		t.Fatalf("unexpected %+v", j)
	}
	h := j.Segments[0].Header
	if h.State != HeaderSynced || h.RecordCount != 2 || h.Nonce != 1234 || h.InitialPages != 5 || h.SectorSize != testSector || h.PageSize != testPageSize {
		t.Fatalf("header %+v", h)
	}
	if !j.LooksHot() {
		t.Fatal("expected hot")
	}
	p := j.Playback()
	if !slices.Equal(p.Pages, []uint32{3, 1}) || p.TruncateTo != 5 || p.StopReason != "end of journal" {
		t.Fatalf("playback %+v", p)
	}
	if j.Segments[0].Records[0].Image[0] != 0xaa || j.Segments[0].Records[0].Offset != testSector {
		t.Fatalf("record %+v", j.Segments[0].Records[0].Offset)
	}
}

// TestNotYetSyncedJournalIsNotHot verifies a pre-sync header with zeroed magic and count is parsed as cold.
func TestNotYetSyncedJournalIsNotHot(t *testing.T) {
	b := &builder{}
	b.header(0, false, 0, 99, 4)
	b.record(2, 1, 99, false)
	b.record(4, 2, 99, false)
	j, _ := Parse(b.data)
	if j.LooksHot() || j.Segments[0].Header.State != HeaderNotYetSynced || j.RecordsPresent() != 2 {
		t.Fatalf("unexpected %+v", j.Segments[0].Header)
	}
	if p := j.Playback(); p.Hot || len(p.Pages) != 0 {
		t.Fatalf("playback %+v", p)
	}
}

// TestSyncedHeaderCountingZeroRecordsRestoresNothing verifies a synced header with RecordCount 0 ignores trailing records.
func TestSyncedHeaderCountingZeroRecordsRestoresNothing(t *testing.T) {
	b := &builder{}
	b.header(0, true, 0, 5, 4)
	b.record(2, 1, 5, false)
	j, _ := Parse(b.data)
	if p := j.Playback(); len(p.Pages) != 0 || j.RecordsPresent() != 0 {
		t.Fatalf("playback %+v present %d", p, j.RecordsPresent())
	}
}

// TestNoSyncJournalCountsRecordsFromSize verifies a NoSyncRecordCount header reads all complete records and records trailing bytes.
func TestNoSyncJournalCountsRecordsFromSize(t *testing.T) {
	b := &builder{}
	b.header(0, true, NoSyncRecordCount, 8, 3)
	b.record(1, 1, 8, false)
	b.record(2, 2, 8, false)
	b.record(3, 3, 8, false)
	b.data = append(b.data, 1, 2, 3)
	j, _ := Parse(b.data)
	if j.Segments[0].Header.State != HeaderNoSync || j.RecordsPresent() != 3 || j.TrailingByte != 3 {
		t.Fatalf("unexpected %+v trailing %d", j.Segments[0].Header, j.TrailingByte)
	}
	if p := j.Playback(); !slices.Equal(p.Pages, []uint32{1, 2, 3}) {
		t.Fatalf("playback %+v", p)
	}
}

// TestPlaybackSkipsPagesBeyondInitialSizeAndStopsAtBadChecksum verifies playback skips out-of-range pages and halts at the first bad checksum.
func TestPlaybackSkipsPagesBeyondInitialSizeAndStopsAtBadChecksum(t *testing.T) {
	b := &builder{}
	b.header(0, true, NoSyncRecordCount, 1, 2)
	b.record(1, 1, 1, false)
	b.record(9, 2, 1, true)
	b.record(2, 3, 1, true)
	b.record(1, 4, 1, false)
	j, _ := Parse(b.data)
	p := j.Playback()
	if !slices.Equal(p.Pages, []uint32{1}) || !slices.Equal(p.SkippedPages, []uint32{9}) || j.ChecksumFailures() != 2 {
		t.Fatalf("playback %+v", p)
	}
	if p.StopReason == "end of journal" {
		t.Fatal("expected a checksum stop")
	}
}

// TestShortJournalCountingMoreRecordsThanPresent verifies playback restores complete records and reports a short journal.
func TestShortJournalCountingMoreRecordsThanPresent(t *testing.T) {
	b := &builder{}
	b.header(0, true, 3, 1, 4)
	b.record(1, 1, 1, false)
	b.data = append(b.data, 0, 0, 0)
	j, _ := Parse(b.data)
	p := j.Playback()
	if !slices.Equal(p.Pages, []uint32{1}) || p.StopReason == "end of journal" {
		t.Fatalf("playback %+v", p)
	}
}

// TestMultipleSegments verifies parsing and playback across two sector-aligned segments with distinct nonces.
func TestMultipleSegments(t *testing.T) {
	b := &builder{}
	b.header(0, true, 1, 7, 6)
	b.record(2, 1, 7, false)
	second := ((len(b.data)-1)/testSector + 1) * testSector
	b.header(second, true, 1, 9, 6)
	b.data = b.data[:second+testSector]
	b.record(5, 2, 9, false)
	j, _ := Parse(b.data)
	if len(j.Segments) != 2 || j.Segments[1].Header.Offset != int64(second) || j.Segments[1].Header.Nonce != 9 {
		t.Fatalf("segments %+v", j.Segments)
	}
	if p := j.Playback(); !slices.Equal(p.Pages, []uint32{2, 5}) {
		t.Fatalf("playback %+v", p)
	}
}

// TestZeroedAndEmptyAndTinyJournals verifies handling of zeroed (PERSIST), empty, and shorter-than-header journals.
func TestZeroedAndEmptyAndTinyJournals(t *testing.T) {
	j, _ := Parse(make([]byte, 4096))
	if j.LooksHot() || j.Segments[0].Header.State != HeaderZeroed {
		t.Fatalf("zeroed %+v", j.Segments)
	}
	j, _ = Parse(nil)
	if j.LooksHot() || len(j.Segments) != 0 {
		t.Fatalf("empty %+v", j)
	}
	j, _ = Parse([]byte{1, 2, 3})
	if !j.LooksHot() || j.Segments[0].Header.State != HeaderMissing {
		t.Fatalf("tiny %+v", j.Segments)
	}
	if p := j.Playback(); len(p.Pages) != 0 {
		t.Fatalf("tiny playback %+v", p)
	}
}

// TestInvalidGeometry verifies Parse marks a header with a non-power-of-two page size as HeaderInvalid.
func TestInvalidGeometry(t *testing.T) {
	b := &builder{}
	b.header(0, true, 1, 1, 1)
	binary.BigEndian.PutUint32(b.data[24:], 1000)
	j, _ := Parse(b.data)
	if j.Segments[0].Header.State != HeaderInvalid || j.RecordsPresent() != 0 {
		t.Fatalf("header %+v", j.Segments[0].Header)
	}
}

// TestSuperJournalName verifies extraction of a multi-database super-journal trailer and PAGER_SJ_PGNO calculation.
func TestSuperJournalName(t *testing.T) {
	b := &builder{}
	b.header(0, true, 1, 3, 2)
	b.record(1, 1, 3, false)
	name := []byte("db-mj0123")
	b.data = binary.BigEndian.AppendUint32(b.data, SuperJournalPage(testPageSize))
	b.data = append(b.data, name...)
	var sum uint32
	for _, c := range name {
		sum += uint32(c)
	}
	b.data = binary.BigEndian.AppendUint32(b.data, uint32(len(name)))
	b.data = binary.BigEndian.AppendUint32(b.data, sum)
	b.data = append(b.data, Magic[:]...)
	j, _ := Parse(b.data)
	if j.SuperJournal != "db-mj0123" || j.RecordsPresent() != 1 || j.TrailingByte != 0 {
		t.Fatalf("super %q present %d trailing %d", j.SuperJournal, j.RecordsPresent(), j.TrailingByte)
	}
	if SuperJournalPage(4096) != 262145 {
		t.Fatal("PAGER_SJ_PGNO for 4096-byte pages is 262145")
	}
}

// TestStateNames verifies HeaderState.String returns the expected labels for known and unknown values.
func TestStateNames(t *testing.T) {
	for state, want := range map[HeaderState]string{HeaderSynced: "synced", HeaderNoSync: "no-sync", HeaderNotYetSynced: "not-yet-synced", HeaderZeroed: "zeroed", HeaderInvalid: "invalid", HeaderMissing: "missing", HeaderState(42): "HeaderState(42)"} {
		if state.String() != want {
			t.Fatalf("%d: %s", int(state), state.String())
		}
	}
}

func liveJournal(t *testing.T, synchronous string) (*Journal, []byte, uint32) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	options := sqlitex.DefaultOptions()
	options.File.JournalMode = sqlitex.JournalDelete
	options.Conn.Synchronous = synchronous
	options.SingleConnection = true
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v BLOB); WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40) INSERT INTO t SELECT i, zeroblob(900) FROM n"); err != nil {
		t.Fatal(err)
	}
	var pages uint32
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// Close the SQLite connection before reading path directly so the test
	// never opens and closes the database file while a connection holds it.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err = sqlitex.Open(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE t SET v = randomblob(900) WHERE id % 3 = 0; INSERT INTO t VALUES(1000, zeroblob(3000))"); err != nil {
		t.Fatal(err)
	}
	// Reading the -journal file while the SQLite connection is open is safe on
	// POSIX because SQLite places advisory locks only on the main database file.
	j, err := ReadFile(path + "-journal")
	if err != nil {
		t.Fatal(err)
	}
	return j, before, pages
}

// TestLiveJournalOfAnOpenTransaction verifies Parse and Playback against a real SQLite rollback journal at FULL and OFF.
func TestLiveJournalOfAnOpenTransaction(t *testing.T) {
	for _, synchronous := range []string{sqlitex.SyncFull, sqlitex.SyncOff} {
		t.Run(synchronous, func(t *testing.T) {
			j, before, pages := liveJournal(t, synchronous)
			h := j.Segments[0].Header
			wantState := HeaderNotYetSynced
			if synchronous == sqlitex.SyncOff {
				wantState = HeaderNoSync
			}
			if h.State != wantState || h.PageSize != 4096 || h.SectorSize != 512 || h.InitialPages != pages {
				t.Fatalf("header %+v", h)
			}
			if j.RecordsPresent() == 0 || j.ChecksumFailures() != 0 {
				t.Fatalf("present %d failures %d", j.RecordsPresent(), j.ChecksumFailures())
			}
			for _, r := range j.Segments[0].Records {
				if r.Page == 0 || r.Page > pages {
					t.Fatalf("record for page %d outside the original %d pages", r.Page, pages)
				}
				original := before[int64(r.Page-1)*4096 : int64(r.Page)*4096]
				if !bytes.Equal(original, r.Image) {
					t.Fatalf("record for page %d is not the original page image", r.Page)
				}
			}
			if synchronous == sqlitex.SyncOff {
				if p := j.Playback(); len(p.Pages) != j.RecordsPresent() {
					t.Fatalf("no-sync playback %+v", p)
				}
			} else if j.LooksHot() {
				t.Fatal("an unsynced journal must not look hot")
			}
		})
	}
}

func syncedHeaderOnly(size int, magic bool) []byte {
	b := &builder{}
	records := uint32(0)
	if magic {
		records = 1
	}
	b.header(0, magic, records, 0x12345678, 1)
	return b.data[:size]
}

// TestJournalShorterThanItsFirstSector verifies journals shorter than one full sector are HeaderMissing with non-negative TrailingByte and no predicted truncation.
func TestJournalShorterThanItsFirstSector(t *testing.T) {
	for _, tc := range []struct {
		size  int
		magic bool
	}{{100, true}, {28, false}, {28, true}, {testSector - 1, true}} {
		j, _ := Parse(syncedHeaderOnly(tc.size, tc.magic))
		h := j.Segments[0].Header
		if h.State != HeaderMissing || j.TrailingByte != int64(tc.size) || j.RecordsPresent() != 0 {
			t.Fatalf("size %d: header %+v trailing %d", tc.size, h, j.TrailingByte)
		}
		if p := j.Playback(); p.TruncateTo != 0 || len(p.Pages) != 0 {
			t.Fatalf("size %d: playback %+v", tc.size, p)
		}
	}
	j, _ := Parse(syncedHeaderOnly(testSector, true))
	if j.Segments[0].Header.State != HeaderSynced || j.TrailingByte != 0 || j.Playback().TruncateTo != 1 {
		t.Fatalf("full sector: %+v trailing %d", j.Segments[0].Header, j.TrailingByte)
	}
}

// TestTrailingByteNeverNegative verifies Parse keeps 0 <= TrailingByte <= len(data) on every prefix of a two-segment journal.
func TestTrailingByteNeverNegative(t *testing.T) {
	b := &builder{}
	b.header(0, true, 2, 9, 4)
	b.record(2, 1, 9, false)
	b.record(3, 2, 9, false)
	b.header(roundUpInt(len(b.data), testSector), true, 1, 9, 4)
	b.record(4, 3, 9, false)
	for size := 0; size <= len(b.data); size++ {
		j, _ := Parse(b.data[:size])
		if j.TrailingByte < 0 || j.TrailingByte > int64(size) {
			t.Fatalf("size %d: trailing %d", size, j.TrailingByte)
		}
	}
}

func roundUpInt(n, unit int) int {
	return (n + unit - 1) / unit * unit
}

func superJournalFooterBytes(name []byte, sum uint32) []byte {
	out := binary.BigEndian.AppendUint32(nil, SuperJournalPage(testPageSize))
	out = append(out, name...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(name)))
	out = binary.BigEndian.AppendUint32(out, sum)
	return append(out, Magic[:]...)
}

func signedSum(name []byte) uint32 {
	return superJournalChecksum(name, true)
}

func platformSum(name []byte) uint32 {
	return superJournalChecksum(name, cCharIsSigned)
}

// TestSuperJournalChecksumFollowsCCharSignedness verifies non-ASCII super-journal checksums match the host platform's C char signedness and reject the opposite signedness.
func TestSuperJournalChecksumFollowsCCharSignedness(t *testing.T) {
	name := []byte("/tmp/caf\xc3\xa9/a.db-mjF47C1696E")
	b := &builder{}
	b.header(0, true, 1, 3, 2)
	b.record(1, 1, 3, false)
	b.data = append(b.data, superJournalFooterBytes(name, platformSum(name))...)
	j, _ := Parse(b.data)
	if j.SuperJournal != string(name) || j.RecordsPresent() != 1 || j.TrailingByte != 0 {
		t.Fatalf("super %q present %d trailing %d", j.SuperJournal, j.RecordsPresent(), j.TrailingByte)
	}
	b2 := &builder{}
	b2.header(0, true, 1, 3, 2)
	b2.record(1, 1, 3, false)
	b2.data = append(b2.data, superJournalFooterBytes(name, superJournalChecksum(name, !cCharIsSigned))...)
	if j, _ := Parse(b2.data); j.SuperJournal != "" {
		t.Fatalf("a checksum of the other char signedness must not be accepted, got %q", j.SuperJournal)
	}
	if superJournalChecksum(name, true) != superJournalChecksum(name, false)-2*256 {
		t.Fatalf("signed %#x unsigned %#x", superJournalChecksum(name, true), superJournalChecksum(name, false))
	}
}

// TestSqliteWrittenSuperJournalChecksum checks Parse against a 45-byte non-ASCII super-journal footer captured from SQLite on signed-char amd64 (0x0d98).
func TestSqliteWrittenSuperJournalChecksum(t *testing.T) {
	name := []byte("/tmp/opencode/caf\xc3\xa9-f_6pfgpk/a.db-mjF47C1696E")
	if !cCharIsSigned {
		t.Skip("the captured footer was written where C char is signed")
	}
	if len(name) != 45 || signedSum(name) != 0x0d98 {
		t.Fatalf("len %d sum %#x", len(name), signedSum(name))
	}
	b := &builder{}
	b.header(0, true, 1, 3, 2)
	b.record(1, 1, 3, false)
	b.data = append(b.data, superJournalFooterBytes(name, 0x0d98)...)
	if j, _ := Parse(b.data); j.SuperJournal != string(name) {
		t.Fatalf("got %q", j.SuperJournal)
	}
}

// TestSuperJournalNameStartingWithNulIsIgnored verifies footers whose first name byte is NUL are rejected (src/pager.c:1337).
func TestSuperJournalNameStartingWithNulIsIgnored(t *testing.T) {
	name := []byte{0, 0, 0, 0}
	b := &builder{}
	b.header(0, true, 1, 3, 2)
	b.record(1, 1, 3, false)
	recordsEnd := len(b.data)
	b.data = append(b.data, superJournalFooterBytes(name, 0)...)
	j, _ := Parse(b.data)
	if j.SuperJournal != "" || j.TrailingByte != int64(len(b.data)-recordsEnd) {
		t.Fatalf("super %q trailing %d", j.SuperJournal, j.TrailingByte)
	}
}

// TestSuperJournalNameTooLongIsIgnored verifies footers longer than maxSuperJournalLen (512 bytes) are ignored while 512 bytes is accepted.
func TestSuperJournalNameTooLongIsIgnored(t *testing.T) {
	name := bytes.Repeat([]byte{'a'}, 513)
	b := &builder{}
	b.header(0, true, 1, 3, 2)
	b.record(1, 1, 3, false)
	b.data = append(b.data, superJournalFooterBytes(name, platformSum(name))...)
	if j, _ := Parse(b.data); j.SuperJournal != "" {
		t.Fatalf("got %q", j.SuperJournal)
	}
	name = name[:512]
	b2 := &builder{}
	b2.header(0, true, 1, 3, 2)
	b2.record(1, 1, 3, false)
	b2.data = append(b2.data, superJournalFooterBytes(name, platformSum(name))...)
	if j, _ := Parse(b2.data); j.SuperJournal != string(name) {
		t.Fatalf("512-byte name rejected")
	}
}

// TestPlaybackWithSuperJournalIsConditional verifies Playback refuses unconditional rollback when SuperJournal is set while PlaybackIfSuperJournalExists predicts the restore.
func TestPlaybackWithSuperJournalIsConditional(t *testing.T) {
	name := []byte("db-mj0123")
	b := &builder{}
	b.header(0, true, 1, 3, 2)
	b.record(1, 1, 3, false)
	b.data = append(b.data, superJournalFooterBytes(name, platformSum(name))...)
	j, _ := Parse(b.data)
	p := j.Playback()
	if !p.Hot || len(p.Pages) != 0 || p.TruncateTo != 0 || !strings.Contains(p.StopReason, "db-mj0123") {
		t.Fatalf("unconditional playback %+v", p)
	}
	if p := j.PlaybackIfSuperJournalExists(); !slices.Equal(p.Pages, []uint32{1}) || p.TruncateTo != 2 {
		t.Fatalf("conditional playback %+v", p)
	}
}

// TestSuperJournalChecksumWhereCCharIsUnsigned verifies the unsigned-char checksum path (used on Linux ARM64) by toggling cCharIsSigned.
func TestSuperJournalChecksumWhereCCharIsUnsigned(t *testing.T) {
	saved := cCharIsSigned
	t.Cleanup(func() { cCharIsSigned = saved })
	name := []byte("/tmp/opencode/caf\xc3\xa9-f_6pfgpk/a.db-mjF47C1696E")
	for _, signed := range []bool{true, false} {
		cCharIsSigned = signed
		b := &builder{}
		b.header(0, true, 1, 3, 2)
		b.record(1, 1, 3, false)
		b.data = append(b.data, superJournalFooterBytes(name, superJournalChecksum(name, signed))...)
		if j, _ := Parse(b.data); j.SuperJournal != string(name) {
			t.Fatalf("signed=%v: got %q", signed, j.SuperJournal)
		}
	}
	if superJournalChecksum(name, false) != 0x0f98 {
		t.Fatalf("unsigned sum %#x", superJournalChecksum(name, false))
	}
}
