package walfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testFrame struct {
	page       uint32
	commitSize uint32
	fill       byte
}

type testWAL struct {
	bigEndian bool
	pageSize  int
	sequence  uint32
	salt1     uint32
	salt2     uint32
	frames    []testFrame
}

func pageImage(pageSize int, page uint32, fill byte) []byte {
	image := bytes.Repeat([]byte{fill}, pageSize)
	binary.BigEndian.PutUint32(image[pageSize-4:], page)
	return image
}

func basePage1(pageSize int, fill byte) []byte {
	image := pageImage(pageSize, 1, fill)
	copy(image, databaseMagic)
	if pageSize == 65536 {
		image[16], image[17] = 0, 1
	} else {
		image[16], image[17] = byte(pageSize>>8), byte(pageSize)
	}
	return image
}

func (w testWAL) build() []byte {
	magic := uint32(MagicLittleEndian)
	if w.bigEndian {
		magic = MagicBigEndian
	}
	header := make([]byte, HeaderSize)
	be := binary.BigEndian
	be.PutUint32(header[0:], magic)
	be.PutUint32(header[4:], FormatVersion)
	be.PutUint32(header[8:], uint32(w.pageSize))
	be.PutUint32(header[12:], w.sequence)
	be.PutUint32(header[16:], w.salt1)
	be.PutUint32(header[20:], w.salt2)
	chain := sumWordPairs(w.bigEndian, header[:24], Checksum{})
	be.PutUint32(header[24:], chain[0])
	be.PutUint32(header[28:], chain[1])
	out := append([]byte(nil), header...)
	for _, f := range w.frames {
		frameHeader := make([]byte, FrameHeaderSize)
		be.PutUint32(frameHeader[0:], f.page)
		be.PutUint32(frameHeader[4:], f.commitSize)
		copy(frameHeader[8:16], header[16:24])
		var image []byte
		if f.page == 1 {
			image = basePage1(w.pageSize, f.fill)
		} else {
			image = pageImage(w.pageSize, f.page, f.fill)
		}
		chain = sumWordPairs(w.bigEndian, frameHeader[:8], chain)
		chain = sumWordPairs(w.bigEndian, image, chain)
		be.PutUint32(frameHeader[16:], chain[0])
		be.PutUint32(frameHeader[20:], chain[1])
		out = append(out, frameHeader...)
		out = append(out, image...)
	}
	return out
}

func threeTransactions(bigEndian bool, pageSize int) testWAL {
	return testWAL{
		bigEndian: bigEndian, pageSize: pageSize, salt1: 0x11223344, salt2: 0x55667788,
		frames: []testFrame{
			{page: 1, fill: 0xa1}, {page: 2, fill: 0xa2}, {page: 3, commitSize: 3, fill: 0xa3},
			{page: 2, commitSize: 3, fill: 0xb2},
			{page: 4, fill: 0xc4}, {page: 1, commitSize: 4, fill: 0xc1},
		},
	}
}

func baseImage(pageSize, pages int) []byte {
	var out []byte
	out = append(out, basePage1(pageSize, 0x01)...)
	for p := 2; p <= pages; p++ {
		out = append(out, pageImage(pageSize, uint32(p), 0x01)...)
	}
	return out
}

func expectedPage(t *testing.T, out []byte, pageSize int, page uint32, fill byte) {
	t.Helper()
	want := pageImage(pageSize, page, fill)
	if page == 1 {
		want = basePage1(pageSize, fill)
	}
	got := out[int(page-1)*pageSize : int(page)*pageSize]
	if !bytes.Equal(got, want) {
		t.Fatalf("page %d holds the wrong image", page)
	}
}

// TestChecksumMatchesSQLiteHeader checks ChecksumBytes against a real SQLite WAL header vector and rejects bad lengths.
func TestChecksumMatchesSQLiteHeader(t *testing.T) {
	header := []byte{0x37, 0x7f, 0x06, 0x82, 0x00, 0x2d, 0xe2, 0x18, 0x00, 0x00, 0x10, 0x00, 0, 0, 0, 0,
		0xc1, 0xab, 0x48, 0x9b, 0xb9, 0xfd, 0x0c, 0xf3}
	sum, err := ChecksumBytes(false, header, Checksum{})
	if err != nil {
		t.Fatal(err)
	}
	if sum != (Checksum{0x702faed4, 0x1b248432}) {
		t.Fatalf("checksum %s", sum)
	}
	if _, err := ChecksumBytes(false, header[:12], Checksum{}); err == nil {
		t.Fatal("a 12-byte input must be refused")
	}
	if _, err := ChecksumBytes(true, nil, Checksum{}); err == nil {
		t.Fatal("an empty input must be refused")
	}
}

// TestParseBothByteOrders verifies Parse, commits, page images, and tuples for both checksum orders and 512/4096/65536 pages.
func TestParseBothByteOrders(t *testing.T) {
	for _, bigEndian := range []bool{false, true} {
		for _, pageSize := range []int{512, 4096, 65536} {
			data := threeTransactions(bigEndian, pageSize).build()
			file, err := Parse(data)
			if err != nil {
				t.Fatalf("big=%v page=%d: %v", bigEndian, pageSize, err)
			}
			if file.Header.BigEndianChecksums() != bigEndian || file.PageSize() != pageSize {
				t.Fatalf("header %s", file.Header)
			}
			if len(file.Frames) != 6 || file.ValidFrames != 6 || file.LastCommitFrame != 6 || file.PartialFrameBytes != 0 {
				t.Fatalf("frames=%d valid=%d last=%d", len(file.Frames), file.ValidFrames, file.LastCommitFrame)
			}
			commits := file.Commits()
			if len(commits) != 3 || commits[0] != (Commit{1, 3, 3}) || commits[1] != (Commit{4, 4, 3}) || commits[2] != (Commit{5, 6, 4}) {
				t.Fatalf("commits %+v", commits)
			}
			if commits[2].Frames() != 2 || file.DatabasePages() != 4 {
				t.Fatalf("commit frames %d pages %d", commits[2].Frames(), file.DatabasePages())
			}
			if int64(len(data)) != HeaderSize+6*int64(pageSize+FrameHeaderSize) {
				t.Fatal("size formula")
			}
			image, err := file.PageImage(4)
			if err != nil || !bytes.Equal(image, pageImage(pageSize, 2, 0xb2)) {
				t.Fatalf("page image of frame 4: %v", err)
			}
			digest := sha256.Sum256(image)
			if file.Tuples(true)[3].PageSHA256 != strings.ToLower(hexString(digest[:])) {
				t.Fatal("tuple hash")
			}
			if _, err := file.PageImage(7); err == nil {
				t.Fatal("frame 7 does not exist")
			}
		}
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// TestByteOrderMatters checks that flipping the magic byte-order bit breaks the header checksum.
func TestByteOrderMatters(t *testing.T) {
	data := threeTransactions(false, 512).build()
	data[3] = 0x83
	_, err := Parse(data)
	if !errors.Is(err, ErrWALIgnored) || !strings.Contains(err.Error(), "header checksum") {
		t.Fatalf("a little-endian WAL relabelled big-endian must fail its header checksum: %v", err)
	}
}

// TestEmptyAndHeaderOnly checks that 0-byte and 32-byte WALs parse cleanly with zero frames.
func TestEmptyAndHeaderOnly(t *testing.T) {
	file, err := Parse(nil)
	if err != nil || len(file.Frames) != 0 || file.Size != 0 {
		t.Fatalf("empty: %v", err)
	}
	header := threeTransactions(false, 4096).build()[:HeaderSize]
	file, err = Parse(header)
	if err != nil || len(file.Frames) != 0 || file.LastCommitFrame != 0 {
		t.Fatalf("header only: %v", err)
	}
}

// TestIgnoredHeaders checks every ErrWALIgnored header case and ErrUnsupportedVersion.
func TestIgnoredHeaders(t *testing.T) {
	good := threeTransactions(false, 1024).build()
	cases := map[string]func([]byte) []byte{
		"shorter than": func(d []byte) []byte { return d[:20] },
		"magic":        func(d []byte) []byte { d[0] = 0x38; return d },
		"page size":    func(d []byte) []byte { binary.BigEndian.PutUint32(d[8:], 1000); return d },
		"page size ":   func(d []byte) []byte { binary.BigEndian.PutUint32(d[8:], 131072); return d },
		"page size  ":  func(d []byte) []byte { binary.BigEndian.PutUint32(d[8:], 256); return d },
		"checksum":     func(d []byte) []byte { d[20] ^= 1; return d },
	}
	for want, edit := range cases {
		_, err := Parse(edit(bytes.Clone(good)))
		var headerErr *HeaderError
		if !errors.Is(err, ErrWALIgnored) || !errors.As(err, &headerErr) || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Fatalf("%s: %v", want, err)
		}
	}
	versioned := testWAL{pageSize: 512, frames: []testFrame{{page: 1, commitSize: 1}}}.build()
	binary.BigEndian.PutUint32(versioned[4:], 3007001)
	chain := sumWordPairs(false, versioned[:24], Checksum{})
	binary.BigEndian.PutUint32(versioned[24:], chain[0])
	binary.BigEndian.PutUint32(versioned[28:], chain[1])
	if _, err := Parse(versioned); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version: %v", err)
	}
}

// TestStopsAtFirstInvalidFrame checks checksum, salt, page-zero, and trailing partial-frame handling.
func TestStopsAtFirstInvalidFrame(t *testing.T) {
	w := threeTransactions(false, 512)
	frameSize := 512 + FrameHeaderSize
	offsetOf := func(frame int) int { return HeaderSize + (frame-1)*frameSize }

	data := w.build()
	data[offsetOf(5)+FrameHeaderSize+100] ^= 0xff
	file, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if file.ValidFrames != 4 || file.LastCommitFrame != 4 || file.StopReason() != FrameChecksumMismatch || file.Frames[5].Problem != FrameAfterInvalid {
		t.Fatalf("checksum: valid=%d last=%d stop=%s", file.ValidFrames, file.LastCommitFrame, file.StopReason())
	}

	data = w.build()
	binary.BigEndian.PutUint32(data[offsetOf(4)+8:], 0x99999999)
	file, _ = Parse(data)
	if file.ValidFrames != 3 || file.StopReason() != FrameSaltMismatch || file.Frames[3].SaltsMatchHeader {
		t.Fatalf("salt: valid=%d stop=%s", file.ValidFrames, file.StopReason())
	}

	data = w.build()
	binary.BigEndian.PutUint32(data[offsetOf(2):], 0)
	file, _ = Parse(data)
	if file.ValidFrames != 1 || file.LastCommitFrame != 0 || file.StopReason() != FramePageZero {
		t.Fatalf("page zero: valid=%d stop=%s", file.ValidFrames, file.StopReason())
	}

	data = append(w.build(), make([]byte, 100)...)
	file, _ = Parse(data)
	if file.PartialFrameBytes != 100 || len(file.Frames) != 6 || file.StopReason() != FrameValid {
		t.Fatalf("partial: %d", file.PartialFrameBytes)
	}
}

// TestOlderGenerationTail verifies Parse and Replay stop at leftover frames from a prior WAL generation.
func TestOlderGenerationTail(t *testing.T) {
	old := threeTransactions(false, 512)
	old.salt1 = 7
	oldData := old.build()
	fresh := testWAL{pageSize: 512, salt1: 8, salt2: 0xabcdef, sequence: 1, frames: []testFrame{{page: 2, commitSize: 4, fill: 0xdd}}}
	freshData := fresh.build()
	data := append(bytes.Clone(freshData), oldData[len(freshData):]...)
	file, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Frames) != 6 || file.ValidFrames != 1 || file.LastCommitFrame != 1 || file.StopReason() != FrameSaltMismatch {
		t.Fatalf("valid=%d stop=%s", file.ValidFrames, file.StopReason())
	}
	base := baseImage(512, 4)
	out, report, err := Replay(base, data)
	if err != nil {
		t.Fatal(err)
	}
	if report.AppliedFrames != 1 || report.IgnoredFrameCount() != 5 || len(report.Ignored) != 2 {
		t.Fatalf("report %+v", report)
	}
	if !strings.Contains(report.Ignored[0].String(), "salt mismatch") || !strings.Contains(report.Ignored[1].String(), "4 of them carry other salts") {
		t.Fatalf("ignored %v", report.Ignored)
	}
	expectedPage(t, out, 512, 2, 0xdd)
	expectedPage(t, out, 512, 3, 0x01)
}

// TestReplay checks Replay across multiple commits in both checksum byte orders.
func TestReplay(t *testing.T) {
	for _, bigEndian := range []bool{false, true} {
		w := threeTransactions(bigEndian, 1024)
		base := baseImage(1024, 2)
		out, report, err := Replay(base, w.build())
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 4*1024 || report.AppliedFrames != 6 || report.DistinctPages != 4 || report.Transactions != 3 || report.IgnoredFrameCount() != 0 {
			t.Fatalf("report %+v", report)
		}
		expectedPage(t, out, 1024, 1, 0xc1)
		expectedPage(t, out, 1024, 2, 0xb2)
		expectedPage(t, out, 1024, 3, 0xa3)
		expectedPage(t, out, 1024, 4, 0xc4)
		if !strings.Contains(strings.Join(report.Lines(), "\n"), "ignored: none") {
			t.Fatal(report.Lines())
		}
	}
}

// TestReplayUncommittedTailAndShrink verifies ignored uncommitted frames and truncation when a commit shrinks the database.
func TestReplayUncommittedTailAndShrink(t *testing.T) {
	w := threeTransactions(false, 512)
	w.frames = append(w.frames, testFrame{page: 1, commitSize: 2, fill: 0xe1}, testFrame{page: 3, fill: 0xf3}, testFrame{page: 5, fill: 0xf5})
	base := baseImage(512, 6)
	out, report, err := Replay(base, w.build())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2*512 || report.LastCommitFrame != 7 || report.DatabasePages != 2 {
		t.Fatalf("len=%d report %+v", len(out), report)
	}
	if report.SkippedBeyondSize != 2 || report.AppliedFrames != 5 {
		t.Fatalf("skipped=%d applied=%d", report.SkippedBeyondSize, report.AppliedFrames)
	}
	if len(report.Ignored) != 1 || report.Ignored[0] != (IgnoredFrames{8, 9, "valid but uncommitted: no commit frame follows"}) {
		t.Fatalf("ignored %v", report.Ignored)
	}
	expectedPage(t, out, 512, 1, 0xe1)
	expectedPage(t, out, 512, 2, 0xb2)
}

// TestReplayWithoutCommitReturnsBase checks that uncommitted, ignored, or empty WALs leave base unchanged.
func TestReplayWithoutCommitReturnsBase(t *testing.T) {
	w := testWAL{pageSize: 512, frames: []testFrame{{page: 2, fill: 9}}}
	base := baseImage(512, 3)
	out, report, err := Replay(base, w.build())
	if err != nil || !bytes.Equal(out, base) || report.IgnoredFrameCount() != 1 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	out, report, err = Replay(base, []byte("garbage that is longer than thirty-two bytes......"))
	if err != nil || !bytes.Equal(out, base) || !strings.Contains(report.WALIgnoredReason, "magic") || !strings.Contains(report.Lines()[0], "WAL ignored as a whole") {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	out, _, err = Replay(base, nil)
	if err != nil || !bytes.Equal(out, base) {
		t.Fatal(err)
	}
}

// TestReplayOntoEmptyBase checks that Replay can materialize a database from an empty base and a WAL.
func TestReplayOntoEmptyBase(t *testing.T) {
	out, report, err := Replay(nil, threeTransactions(false, 512).build())
	if err != nil || len(out) != 4*512 || report.BaseBytes != 0 {
		t.Fatalf("err=%v len=%d", err, len(out))
	}
}

// TestReplayErrors verifies typed errors for page-size mismatch, bad base, bad base size, huge commit size, and version.
func TestReplayErrors(t *testing.T) {
	wal := threeTransactions(false, 1024).build()
	if _, _, err := Replay(baseImage(512, 2), wal); !errors.Is(err, ErrPageSizeMismatch) {
		t.Fatalf("page size: %v", err)
	}
	if _, _, err := Replay(make([]byte, 2048), wal); !errors.Is(err, ErrBaseNotDatabase) {
		t.Fatalf("not a database: %v", err)
	}
	if _, _, err := Replay(append(baseImage(1024, 2), 1, 2, 3), wal); !errors.Is(err, ErrBaseSize) {
		t.Fatalf("base size: %v", err)
	}
	huge := testWAL{pageSize: 512, frames: []testFrame{{page: 1, commitSize: 0xfffffff0}}}.build()
	if _, _, err := Replay(baseImage(512, 1), huge); !errors.Is(err, ErrCommitSizeTooLarge) {
		t.Fatalf("commit size: %v", err)
	}
	versioned := bytes.Clone(wal)
	versioned[4] = 9
	chain := sumWordPairs(false, versioned[:24], Checksum{})
	binary.BigEndian.PutUint32(versioned[24:], chain[0])
	binary.BigEndian.PutUint32(versioned[28:], chain[1])
	if _, _, err := Replay(baseImage(1024, 2), versioned); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version: %v", err)
	}
}

// TestReplayFiles checks atomic file replay, sidecar refusal, missing-WAL handling, and temp-file cleanup.
func TestReplayFiles(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.db")
	walPath := filepath.Join(dir, "base.db-wal")
	if err := os.WriteFile(basePath, baseImage(1024, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(walPath, threeTransactions(false, 1024).build(), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.db")
	report, err := ReplayFiles(basePath, walPath, out)
	if err != nil || report.OutputBytes != 4096 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() != 4096 {
		t.Fatal(err)
	}
	if _, err := ReplayFiles(basePath, walPath, out); !errors.Is(err, ErrOutputExists) {
		t.Fatalf("existing output: %v", err)
	}
	second := filepath.Join(dir, "second.db")
	if err := os.WriteFile(second+"-wal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayFiles(basePath, walPath, second); !errors.Is(err, ErrOutputExists) {
		t.Fatalf("stale side file: %v", err)
	}
	third := filepath.Join(dir, "third.db")
	if _, err := ReplayFiles(filepath.Join(dir, "missing.db"), walPath, third); err == nil {
		t.Fatal("a missing base must be an error")
	}
	if _, err := ReplayFiles(basePath, filepath.Join(dir, "no-wal"), third); err != nil {
		t.Fatalf("a missing WAL means no frames: %v", err)
	}
	bad := filepath.Join(dir, "bad.db")
	if _, err := ReplayFiles(filepath.Join(dir, "base.db-wal"), walPath, bad); err == nil {
		t.Fatal("a WAL as base must be refused")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".replay-tmp") || e.Name() == "bad.db" {
			t.Fatalf("left behind %s", e.Name())
		}
	}
}

// TestCompareBytes checks region classification and salt/checksum-only difference filtering.
func TestCompareBytes(t *testing.T) {
	a := threeTransactions(false, 512)
	b := a
	b.salt1, b.salt2 = 1, 2
	result := CompareBytes(a.build(), b.build(), 512)
	if result.Identical() || !result.OnlyIn(RegionHeaderSalt, RegionHeaderChecksum, RegionFrameSalt, RegionFrameChecksum) {
		t.Fatalf("salts: %s", result)
	}
	if result.ByRegion[RegionHeaderSalt] == 0 || result.ByRegion[RegionFrameSalt] == 0 || result.ByRegion[RegionPageData] != 0 {
		t.Fatalf("regions %s", result)
	}
	c := a
	c.frames = append([]testFrame(nil), a.frames...)
	c.frames[1].fill = 0x77
	result = CompareBytes(a.build(), c.build(), 512)
	if result.ByRegion[RegionPageData] == 0 || result.OnlyIn(RegionFrameChecksum) {
		t.Fatalf("page data: %s", result)
	}
	result = CompareBytes(a.build(), append(a.build(), 1, 2), 512)
	if result.ByRegion[RegionBeyondShorterFile] != 2 || !strings.Contains(result.String(), "sizes") {
		t.Fatalf("size: %s", result)
	}
	if !CompareBytes(a.build(), a.build(), 512).Identical() {
		t.Fatal("identical")
	}
	for offset, want := range map[int64]Region{0: RegionHeaderMagic, 5: RegionHeaderVersion, 9: RegionHeaderPageSize, 13: RegionHeaderCheckpointSequence,
		17: RegionHeaderSalt, 30: RegionHeaderChecksum, 32: RegionFramePageNumber, 36: RegionFrameCommitSize, 40: RegionFrameSalt, 50: RegionFrameChecksum, 56: RegionPageData,
		32 + 536: RegionFramePageNumber} {
		if got := RegionAt(offset, 512); got != want {
			t.Fatalf("offset %d: %s want %s", offset, got, want)
		}
	}
}

// TestEqualTuples checks tuple-sequence equality and 1-based mismatch index reporting.
func TestEqualTuples(t *testing.T) {
	a := []Tuple{{1, "x", 0}, {2, "y", 2}}
	if ok, _ := EqualTuples(a, a); !ok {
		t.Fatal("equal")
	}
	if ok, at := EqualTuples(a, []Tuple{{1, "x", 0}, {2, "z", 2}}); ok || at != 2 {
		t.Fatal("differ at 2")
	}
	if ok, at := EqualTuples(a, a[:1]); ok || at != 2 {
		t.Fatal("length")
	}
}

type testIndex struct {
	bigEndian bool
	header    IndexHeader
	info      CheckpointInfo
	pages     []uint32
	size      int
}

func (x testIndex) build() []byte {
	var order binary.ByteOrder = binary.LittleEndian
	if x.bigEndian {
		order = binary.BigEndian
	}
	data := make([]byte, x.size)
	raw := make([]byte, IndexHeaderSize)
	order.PutUint32(raw[0:], x.header.Version)
	order.PutUint32(raw[8:], x.header.Change)
	raw[12] = x.header.IsInit
	raw[13] = x.header.BigEndianChecksum
	order.PutUint16(raw[14:], x.header.PageSizeField)
	order.PutUint32(raw[16:], x.header.MaxFrame)
	order.PutUint32(raw[20:], x.header.Pages)
	binary.BigEndian.PutUint32(raw[32:], x.header.Salt1)
	binary.BigEndian.PutUint32(raw[36:], x.header.Salt2)
	sum := sumWordPairs(x.bigEndian, raw[:40], Checksum{})
	order.PutUint32(raw[40:], sum[0])
	order.PutUint32(raw[44:], sum[1])
	copy(data[0:], raw)
	copy(data[IndexHeaderSize:], raw)
	info := data[CheckpointInfoOffset:]
	order.PutUint32(info[0:], x.info.Backfill)
	for i, m := range x.info.ReadMarks {
		order.PutUint32(info[4+4*i:], m)
	}
	order.PutUint32(info[32:], x.info.BackfillAttempted)
	for i, p := range x.pages {
		block, offset := indexSlot(uint32(i + 1))
		order.PutUint32(data[block*IndexBlockSize+offset:], p)
	}
	return data
}

// TestParseIndex checks wal-index decoding in both byte orders, 64 KiB page sizes, block boundaries, and error cases.
func TestParseIndex(t *testing.T) {
	for _, bigEndian := range []bool{false, true} {
		pages := make([]uint32, FramesInFirstIndexBlock+3)
		for i := range pages {
			pages[i] = uint32(i%97 + 1)
		}
		x := testIndex{
			bigEndian: bigEndian, size: 2 * IndexBlockSize, pages: pages,
			header: IndexHeader{Version: IndexFormatVersion, Change: 5, IsInit: 1, PageSizeField: 1, MaxFrame: uint32(len(pages)), Pages: 40, Salt1: 0xdeadbeef, Salt2: 0x01020304},
			info:   CheckpointInfo{Backfill: 12, ReadMarks: [5]uint32{0, 12, ReadMarkNotUsed, 30, ReadMarkNotUsed}, BackfillAttempted: 12},
		}
		index, err := ParseIndex(x.build())
		if err != nil {
			t.Fatal(err)
		}
		h := index.Header()
		if !index.Initialized || index.BigEndian != bigEndian || !index.HeadersAgree() || !h.ChecksumOK {
			t.Fatalf("index %+v", index)
		}
		if h.PageSize() != 65536 || h.MaxFrame != uint32(len(pages)) || h.Salt1 != 0xdeadbeef || h.Change != 5 || h.Pages != 40 {
			t.Fatalf("header %s", h)
		}
		if index.Checkpoint.Backfill != 12 || index.Checkpoint.ReadMarks[3] != 30 || !strings.Contains(index.Checkpoint.String(), "[2]=unused [3]=30") {
			t.Fatalf("checkpoint %s", index.Checkpoint)
		}
		got, err := index.FramePages(h.MaxFrame)
		if err != nil || len(got) != len(pages) {
			t.Fatalf("frame pages: %v", err)
		}
		for i := range pages {
			if got[i] != pages[i] {
				t.Fatalf("frame %d page %d want %d", i+1, got[i], pages[i])
			}
		}
		if _, err := index.FramePage(0); !errors.Is(err, ErrFrameNotIndexed) {
			t.Fatal("frame 0")
		}
		if _, err := index.FramePage(FramesInFirstIndexBlock + FramesPerIndexBlock + 1); !errors.Is(err, ErrFrameNotIndexed) {
			t.Fatal("beyond the mapped blocks")
		}
	}
	block, offset := indexSlot(FramesInFirstIndexBlock)
	if block != 0 || offset != IndexBlockSize/2-4 {
		t.Fatalf("last slot of block 0: %d %d", block, offset)
	}
	if block, offset = indexSlot(FramesInFirstIndexBlock + 1); block != 1 || offset != 0 {
		t.Fatalf("first slot of block 1: %d %d", block, offset)
	}
	if FramesInFirstIndexBlock != 4062 || IndexHeaderRegionSize != 136 || DeadManSwitchOffset != 128 {
		t.Fatal("layout constants")
	}
	x := testIndex{size: IndexBlockSize, header: IndexHeader{Version: IndexFormatVersion, PageSizeField: 0x1000}}
	page4096, _ := ParseIndex(x.build())
	if page4096.Header().PageSize() != 4096 {
		t.Fatal("page size 4096")
	}
	data := x.build()
	data[44] ^= 1
	broken, _ := ParseIndex(data)
	if broken.Headers[0].ChecksumOK || !broken.Headers[1].ChecksumOK || broken.HeadersAgree() {
		t.Fatal("checksum detection")
	}
	zero, err := ParseIndex(make([]byte, IndexBlockSize))
	if err != nil || zero.Initialized {
		t.Fatalf("zeroed header: %v", err)
	}
	if _, err := zero.FramePage(1); !errors.Is(err, ErrFrameNotIndexed) {
		t.Fatal("uninitialized index")
	}
	if _, err := ParseIndex(make([]byte, 100)); !errors.Is(err, ErrShortIndex) {
		t.Fatal("short")
	}
	junk := make([]byte, 200)
	junk[0] = 7
	if _, err := ParseIndex(junk); !errors.Is(err, ErrIndexVersion) {
		t.Fatal("version")
	}
}

// TestParseShowwal checks parsing of showwal output including checksum mismatch annotations.
func TestParseShowwal(t *testing.T) {
	text := `Pagesize: 4096
Available pages: 1..3
Checksum byte order: native
WAL Header:
 000: 37 7f 06 82  0x377f0682  Magic.  0x377f0682 (le) or 0x377f0683 (be)
Frame    1:      1      0 0xc1ab489b,b9fd0cf3 0x1dfddad4,f20d71c2
Frame    2:      2      2 0xc1ab489b,b9fd0cf3 0x64e8782e,e8cb7f89
Frame    3:      2      2 0xc1ab489b,b9fd0cf3 0x73371a28,d2bdc887 should be 0x00000001,00000002
`
	listing, err := ParseShowwal(text)
	if err != nil {
		t.Fatal(err)
	}
	if listing.PageSize != 4096 || listing.AvailableFrames != 3 || len(listing.Frames) != 3 {
		t.Fatalf("%+v", listing)
	}
	f := listing.Frames[2]
	if f.Page != 2 || f.CommitSize != 2 || f.Salt1 != 0xc1ab489b || f.StoredChecksum != (Checksum{0x73371a28, 0xd2bdc887}) || f.ChecksumAgrees || f.ExpectedChecksum != (Checksum{1, 2}) {
		t.Fatalf("frame 3 %+v", f)
	}
	if !listing.Frames[0].ChecksumAgrees {
		t.Fatal("frame 1")
	}
	if _, err := ParseShowwal("Frame 1: x"); err == nil {
		t.Fatal("bad frame line")
	}
	if _, err := ParseShowwal("nothing"); err == nil {
		t.Fatal("no page size")
	}
}

// TestCompareWithShowwal checks agreement and discrepancy reporting against a ShowwalListing.
func TestCompareWithShowwal(t *testing.T) {
	file, err := Parse(threeTransactions(false, 512).build())
	if err != nil {
		t.Fatal(err)
	}
	listing := ShowwalListing{PageSize: 512, AvailableFrames: 6}
	for _, f := range file.Frames {
		listing.Frames = append(listing.Frames, ShowwalFrame{Number: f.Number, Page: f.Page, CommitSize: f.CommitSize, Salt1: f.Salt1, Salt2: f.Salt2, StoredChecksum: f.StoredChecksum, ChecksumAgrees: true})
	}
	if problems := file.CompareWithShowwal(listing); len(problems) != 0 {
		t.Fatal(problems)
	}
	listing.Frames[2].Page = 9
	listing.Frames[3].ChecksumAgrees = false
	if problems := file.CompareWithShowwal(listing); len(problems) != 2 {
		t.Fatal(problems)
	}
}

// TestMalformedInputNeverPanics runs 3,000 mutated and 200 random inputs through every parser and Replay.
func TestMalformedInputNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	seeds := [][]byte{threeTransactions(false, 512).build(), threeTransactions(true, 512).build()}
	base := baseImage(512, 3)
	for i := 0; i < 3000; i++ {
		data := bytes.Clone(seeds[i%2])
		for n := rng.IntN(4) + 1; n > 0; n-- {
			data[rng.IntN(len(data))] ^= byte(rng.IntN(255) + 1)
		}
		if rng.IntN(5) == 0 {
			data = data[:rng.IntN(len(data))]
		}
		file, err := Parse(data)
		if err == nil {
			_ = file.Commits()
			_ = file.Tuples(true)
		}
		_, _, _ = Replay(base, data)
		_, _ = ParseIndex(data)
		_, _ = ParseShowwal(string(data))
	}
	for n := 0; n < 200; n++ {
		data := make([]byte, rng.IntN(400))
		for i := range data {
			data[i] = byte(rng.IntN(256))
		}
		_, _ = Parse(data)
		_, _, _ = Replay(data, data)
		if index, err := ParseIndex(data); err == nil {
			_, _ = index.FramePages(index.Header().MaxFrame)
		}
	}
}

// FuzzParseAndReplay fuzzes Parse, Replay, and ParseIndex against arbitrary byte slices.
func FuzzParseAndReplay(f *testing.F) {
	f.Add(threeTransactions(false, 512).build())
	f.Add(threeTransactions(true, 512).build())
	base := baseImage(512, 3)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
		_, _, _ = Replay(base, data)
		if index, err := ParseIndex(data); err == nil {
			_, _ = index.FramePages(index.Header().MaxFrame)
		}
	})
}
