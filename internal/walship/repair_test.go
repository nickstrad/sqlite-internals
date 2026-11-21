package walship

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/walfleet/internal/walfile"
)

// TestResumeWithNoWALSeesOfflineCommits verifies that resuming with no WAL detects offline commits whose -wal was deleted by checking the full page hash.
func TestResumeWithNoWALSeesOfflineCommits(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(0, 2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = h.open(0, 2)
	if !s.Resumed() || !s.WALPosition().Generation.None() {
		t.Fatalf("resumed=%t wal=%+v", s.Resumed(), s.WALPosition())
	}
	report := h.cycle(s)
	if report.Change != WALUnchanged || report.Snapshot != nil || report.Segment != nil {
		t.Fatalf("nothing changed while down: %v", report.Lines((&GenerationNames{}).Name))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	h.step(3)
	s = h.open(0, 2)
	report = h.cycle(s)
	if report.Change != WALDiscontinuity || report.Snapshot == nil || report.Snapshot.Position != 3 || report.Snapshot.UserVersion != 3 || !report.Snapshot.WAL.Generation.None() {
		t.Fatalf("step 3 committed and checkpointed away while down: %v", report.Lines((&GenerationNames{}).Name))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	h.step(4)
	s = h.open(0, 2)
	report = h.cycle(s)
	if report.Change != WALDiscontinuity || report.Snapshot == nil || report.Snapshot.Position != 4 || report.Snapshot.UserVersion != 4 {
		t.Fatalf("step 4 committed while down after a snapshot taken with no WAL: %v", report.Lines((&GenerationNames{}).Name))
	}
	h.step(5)
	report = h.cycle(s)
	if report.Segment == nil || report.Segment.FirstPosition != 5 || report.Segment.Commits[0].UserVersion != 5 {
		t.Fatalf("after the snapshot: %v", report.Lines((&GenerationNames{}).Name))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	h.checkEveryPosition(h.catalog(), 2, 5)
}

// TestResumeRefusesADamagedObjectPastTheLastPosition verifies that Open fails with ErrBrokenChain instead of rewinding past a damaged latest segment.
func TestResumeRefusesADamagedObjectPastTheLastPosition(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(0, 2)
	h.shipSteps(s, 3, 6)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	object, err := h.store.Get(ctx, SegmentKey(6))
	if err != nil {
		t.Fatal(err)
	}
	object[len(object)-100] ^= 0x01
	if err := h.store.Delete(ctx, SegmentKey(6)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutIfAbsent(ctx, SegmentKey(6), object); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, h.config(0, 2))
	if !errors.Is(err, ErrBrokenChain) || !strings.Contains(err.Error(), SegmentKey(6)) {
		t.Fatalf("resume over a damaged latest segment: %v", err)
	}
}

func segmentWithFrames(t *testing.T, h *harness, minFrames int) (string, SegmentMeta, []byte) {
	t.Helper()
	for _, segment := range h.catalog().Segments {
		if segment.Meta.LastFrame-segment.Meta.FirstFrame+1 >= minFrames {
			return segment.Key, segment.Meta, segment.Frames
		}
	}
	t.Fatalf("no segment with %d frames", minFrames)
	return "", SegmentMeta{}, nil
}

func damagedBy(t *testing.T, meta SegmentMeta, frames []byte) string {
	t.Helper()
	encoded, err := EncodeSegment(meta, frames)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &Catalog{}
	catalog.add(SegmentKey(meta.FirstPosition), encoded)
	if len(catalog.Damaged) != 1 {
		t.Fatalf("forged segment accepted: %+v", meta)
	}
	return catalog.Damaged[0].Problem
}

// TestSegmentHeaderAndCommitFramesAreVerifiedBeforeReplay confirms that forged WAL headers, wrong commit page counts, and commit points on non-commit frames are quarantined as Damaged before replay.
func TestSegmentHeaderAndCommitFramesAreVerifiedBeforeReplay(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(0, 2)
	h.shipSteps(s, 3, 5)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, meta, frames := segmentWithFrames(t, h, 2)
	if err := VerifySegmentFrames(meta, frames); err != nil {
		t.Fatalf("intact segment: %v", err)
	}
	header, err := meta.Generation.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	badMagic := meta
	forged := append([]byte(nil), header...)
	forged[0] ^= 0x10
	badMagic.Generation = Generation{Header: hex.EncodeToString(forged)}
	if problem := damagedBy(t, badMagic, frames); !strings.Contains(problem, "segment WAL header") {
		t.Fatalf("bad magic: %s", problem)
	}

	badHeaderChecksum := meta
	forged = append([]byte(nil), header...)
	forged[31] ^= 0x01
	badHeaderChecksum.Generation = Generation{Header: hex.EncodeToString(forged)}
	if problem := damagedBy(t, badHeaderChecksum, frames); !strings.Contains(problem, "segment WAL header") {
		t.Fatalf("bad header checksum: %s", problem)
	}

	wrongPages := meta
	wrongPages.Commits = append([]CommitPoint(nil), meta.Commits...)
	wrongPages.Commits[0].Pages++
	if problem := damagedBy(t, wrongPages, frames); !strings.Contains(problem, "commit size") {
		t.Fatalf("wrong commit pages: %s", problem)
	}

	frameSize := meta.PageSize + walfile.FrameHeaderSize
	if binary.BigEndian.Uint32(frames[4:]) != 0 {
		t.Fatal("the segment's first frame is a commit frame; the test needs a non-commit frame")
	}
	nonCommit := meta
	nonCommit.LastFrame = meta.FirstFrame
	nonCommit.LastChecksum = [2]uint32{binary.BigEndian.Uint32(frames[16:]), binary.BigEndian.Uint32(frames[20:])}
	nonCommit.Commits = []CommitPoint{meta.Commits[len(meta.Commits)-1]}
	nonCommit.Commits[0].Frame = meta.FirstFrame
	nonCommit.Commits[0].Position = meta.FirstPosition
	nonCommit.LastPosition = meta.FirstPosition
	nonCommit.AfterSHA256 = nonCommit.Commits[0].StateSHA256
	if problem := damagedBy(t, nonCommit, frames[:frameSize]); !strings.Contains(problem, "commit size 0") {
		t.Fatalf("commit point on a non-commit frame: %s", problem)
	}
}

type failingDeleteStore struct {
	Store
	deletes, failAt int
}

func (f *failingDeleteStore) Delete(ctx context.Context, key string) error {
	f.deletes++
	if f.deletes == f.failAt {
		return errors.New("delete failed")
	}
	return f.Store.Delete(ctx, key)
}

// TestPruneReportsOnlyWhatItDeleted verifies that Prune returns only the keys deleted before a store.Delete error.
func TestPruneReportsOnlyWhatItDeleted(t *testing.T) {
	h := newHarness(t, 2)
	s := h.open(2, 2)
	h.shipSteps(s, 3, 9)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	store := &failingDeleteStore{Store: h.store, failAt: 3}
	deleted, err := Prune(h.ctx, store, 7)
	if err == nil {
		t.Fatal("the failing delete was not reported")
	}
	if want := []string{SnapshotKey(2), SnapshotKey(4)}; !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted %v, want %v", deleted, want)
	}
}

// TestDecodeSnapshotRefusesImpossibleShapes verifies that DecodeSnapshot rejects zero-page snapshots, non-power-of-two or out-of-range page sizes, and malformed WAL generation headers.
func TestDecodeSnapshotRefusesImpossibleShapes(t *testing.T) {
	image := make([]byte, 4096)
	good := SnapshotMeta{Position: 1, Reason: ReasonInitial, PageSize: 4096, Pages: 1, StateSHA256: SHA256Hex(image)}
	for name, c := range map[string]struct {
		meta SnapshotMeta
		body []byte
	}{
		"no pages":         {SnapshotMeta{Position: 1, Reason: ReasonInitial, PageSize: 4096, StateSHA256: SHA256Hex(nil)}, nil},
		"page size 1000":   {SnapshotMeta{Position: 1, Reason: ReasonInitial, PageSize: 1000, Pages: 1, StateSHA256: SHA256Hex(make([]byte, 1000))}, make([]byte, 1000)},
		"page size 128":    {SnapshotMeta{Position: 1, Reason: ReasonInitial, PageSize: 128, Pages: 1, StateSHA256: SHA256Hex(make([]byte, 128))}, make([]byte, 128)},
		"bad wal header":   {func() SnapshotMeta { m := good; m.WAL.Generation = Generation{Header: "abcd"}; return m }(), image},
		"non-hex wal head": {func() SnapshotMeta { m := good; m.WAL.Generation = Generation{Header: "zz"}; return m }(), image},
	} {
		encoded, err := EncodeSnapshot(c.meta, c.body)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := DecodeSnapshot(encoded); !errors.Is(err, ErrMalformedObject) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	encoded, err := EncodeSnapshot(good, image)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeSnapshot(encoded); err != nil {
		t.Fatalf("good snapshot: %v", err)
	}
}

// TestOpenAcceptsJournalModeInAnyCase confirms that Open matches "wal" case-insensitively and rejects rollback journal modes with ErrNotWAL.
func TestOpenAcceptsJournalModeInAnyCase(t *testing.T) {
	h := newHarness(t, 2)
	config := h.config(0, 2)
	config.Options.File.JournalMode = "WAL"
	s, err := Open(h.ctx, config)
	if err != nil {
		t.Fatalf("journal mode WAL: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	config.Options.File.JournalMode = "delete"
	if _, err := Open(h.ctx, config); !errors.Is(err, ErrNotWAL) {
		t.Fatalf("journal mode delete: %v", err)
	}
}
