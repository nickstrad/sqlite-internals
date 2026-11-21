package walfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var databaseMagic = []byte("SQLite format 3\x00")

// IgnoredFrames summarizes a contiguous range of WAL frames that Replay did not
// apply, along with the reason they were skipped.
type IgnoredFrames struct {
	FirstFrame int
	LastFrame  int
	Reason     string
}

// Count returns the number of frames in g.
func (g IgnoredFrames) Count() int { return g.LastFrame - g.FirstFrame + 1 }

// String formats g as either a single frame or an inclusive frame range.
func (g IgnoredFrames) String() string {
	if g.FirstFrame == g.LastFrame {
		return fmt.Sprintf("frame %d: %s", g.FirstFrame, g.Reason)
	}
	return fmt.Sprintf("frames %d-%d (%d): %s", g.FirstFrame, g.LastFrame, g.Count(), g.Reason)
}

// ReplayReport records every count and ignored-frame group produced by Replay
// or ReplayFiles.
type ReplayReport struct {
	WALIgnoredReason  string
	PageSize          int
	FramesInFile      int
	PartialFrameBytes int64
	ValidFrames       int
	AppliedFrames     int
	Transactions      int
	LastCommitFrame   int
	DatabasePages     uint32
	DistinctPages     int
	SkippedBeyondSize int
	BaseBytes         int64
	OutputBytes       int64
	Ignored           []IgnoredFrames
}

// IgnoredFrameCount returns the total number of whole frames not applied.
func (r ReplayReport) IgnoredFrameCount() int {
	total := 0
	for _, g := range r.Ignored {
		total += g.Count()
	}
	return total
}

// Lines formats r into human-readable summary lines suitable for experiment output.
func (r ReplayReport) Lines() []string {
	if r.WALIgnoredReason != "" {
		return []string{
			fmt.Sprintf("WAL ignored as a whole (%s); output = base (%d bytes)", r.WALIgnoredReason, r.BaseBytes),
		}
	}
	lines := []string{
		fmt.Sprintf("frames in file %d, valid %d, committed transactions %d, last commit frame %d (database size %d pages)",
			r.FramesInFile, r.ValidFrames, r.Transactions, r.LastCommitFrame, r.DatabasePages),
		fmt.Sprintf("applied %d frames (%d distinct pages, %d skipped beyond the committed size); base %d bytes -> output %d bytes",
			r.AppliedFrames, r.DistinctPages, r.SkippedBeyondSize, r.BaseBytes, r.OutputBytes),
	}
	if len(r.Ignored) == 0 && r.PartialFrameBytes == 0 {
		lines = append(lines, "ignored: none")
	}
	for _, g := range r.Ignored {
		lines = append(lines, "ignored "+g.String())
	}
	if r.PartialFrameBytes > 0 {
		lines = append(lines, fmt.Sprintf("ignored %d trailing bytes: a partial frame", r.PartialFrameBytes))
	}
	return lines
}

func basePageSize(base []byte) (int, error) {
	if len(base) == 0 {
		return 0, nil
	}
	if len(base) < 100 || !bytes.Equal(base[:16], databaseMagic) {
		return 0, fmt.Errorf("%w: %d bytes without the \"SQLite format 3\" header", ErrBaseNotDatabase, len(base))
	}
	// SQLite stores page size 65,536 as big-endian 1 in database header bytes 16..17.
	size := int(base[16])<<8 | int(base[17])
	if size == 1 {
		size = 65536
	}
	if size < MinPageSize || size > MaxPageSize || size&(size-1) != 0 {
		return 0, fmt.Errorf("%w: header page size field %d", ErrBaseNotDatabase, size)
	}
	return size, nil
}

func ignoredGroups(file *File) []IgnoredFrames {
	var groups []IgnoredFrames
	if file.ValidFrames > file.LastCommitFrame {
		groups = append(groups, IgnoredFrames{
			FirstFrame: file.LastCommitFrame + 1,
			LastFrame:  file.ValidFrames,
			Reason:     "valid but uncommitted: no commit frame follows",
		})
	}
	if file.ValidFrames < len(file.Frames) {
		stop := file.Frames[file.ValidFrames]
		groups = append(groups, IgnoredFrames{FirstFrame: stop.Number, LastFrame: stop.Number, Reason: stop.Problem.String()})
		if stop.Number < len(file.Frames) {
			rest := IgnoredFrames{FirstFrame: stop.Number + 1, LastFrame: len(file.Frames)}
			older := 0
			for _, frame := range file.Frames[stop.Number:] {
				if !frame.SaltsMatchHeader {
					older++
				}
			}
			rest.Reason = fmt.Sprintf("%s (%d of them carry other salts)", FrameAfterInvalid, older)
			groups = append(groups, rest)
		}
	}
	return groups
}

// Replay applies all committed WAL frames onto base in plain Go, mirroring
// SQLite's checkpoint backfill in walCheckpoint (src/wal.c:2282-2335).
// A non-empty base must carry the 100-byte SQLite header with a power-of-two
// page size in 512..65536 (ErrBaseNotDatabase); page-size mismatch
// (ErrPageSizeMismatch) and non-page-multiple base length (ErrBaseSize) are
// validated even when the WAL has no commit frame (an empty WAL checks base
// length against the base's own page size). Frames are applied in order up to
// LastCommitFrame so later versions of a page overwrite earlier ones; frames
// targeting pages past the final commit size are skipped (src/wal.c:2312), and
// the result is truncated or zero-extended to the final commit size
// (src/wal.c:2328-2331).
func Replay(base, wal []byte) ([]byte, ReplayReport, error) {
	report := ReplayReport{BaseBytes: int64(len(base))}
	pageSize, err := basePageSize(base)
	if err != nil {
		return nil, report, err
	}
	file, err := Parse(wal)
	if errors.Is(err, ErrWALIgnored) {
		report.WALIgnoredReason = err.Error()
		report.OutputBytes = int64(len(base))
		return bytes.Clone(base), report, nil
	}
	if err != nil {
		return nil, report, err
	}
	report.PageSize = file.PageSize()
	report.FramesInFile = len(file.Frames)
	report.PartialFrameBytes = file.PartialFrameBytes
	report.ValidFrames = file.ValidFrames
	report.LastCommitFrame = file.LastCommitFrame
	report.Transactions = len(file.Commits())
	report.DatabasePages = file.DatabasePages()
	report.Ignored = ignoredGroups(file)
	// Validate base page size and whole-page alignment before returning early
	// on zero commits so header-only, uncommitted, and empty WALs still reject
	// mismatched or truncated base files.
	walPageSize := file.PageSize()
	if len(base) > 0 && walPageSize != 0 && pageSize != walPageSize {
		return nil, report, fmt.Errorf("%w: base %d, WAL %d", ErrPageSizeMismatch, pageSize, walPageSize)
	}
	if sizeUnit := max(walPageSize, pageSize); sizeUnit != 0 && len(base)%sizeUnit != 0 {
		return nil, report, fmt.Errorf("%w: %d bytes, page size %d", ErrBaseSize, len(base), sizeUnit)
	}
	if file.LastCommitFrame == 0 {
		report.OutputBytes = int64(len(base))
		return bytes.Clone(base), report, nil
	}
	// Reject impossible commit sizes using walCheckpoint's exact bound
	// (src/wal.c:2288-2293: base + 65536 for a possible pending-byte page +
	// frames*pageSize). This also prevents a corrupt header from triggering a
	// huge allocation.
	finalSize := int64(report.DatabasePages) * int64(walPageSize)
	if limit := int64(len(base)) + 65536 + int64(file.LastCommitFrame)*int64(walPageSize); finalSize > limit {
		return nil, report, fmt.Errorf("%w: commit size %d pages needs %d bytes, more than base %d + 65536 + %d frames x %d", ErrCommitSizeTooLarge, report.DatabasePages, finalSize, len(base), file.LastCommitFrame, walPageSize)
	}
	out := make([]byte, max(int64(len(base)), finalSize))
	copy(out, base)
	distinct := map[uint32]bool{}
	for _, frame := range file.CommittedFrames() {
		if frame.Page > report.DatabasePages {
			report.SkippedBeyondSize++
			continue
		}
		image, err := file.PageImage(frame.Number)
		if err != nil {
			return nil, report, err
		}
		copy(out[int64(frame.Page-1)*int64(walPageSize):], image)
		distinct[frame.Page] = true
		report.AppliedFrames++
	}
	report.DistinctPages = len(distinct)
	out = out[:finalSize]
	report.OutputBytes = finalSize
	return out, report, nil
}

// ReplayFiles replays walPath onto basePath and atomically writes the rebuilt
// database to outPath via a synced temporary file (outPath + ".replay-tmp",
// opened with O_EXCL) and rename. A missing walPath is treated as zero frames.
// Existing outPath or sibling -wal, -journal, or -shm files are refused with
// ErrOutputExists so SQLite cannot later apply stale sidecar files. If
// outPath + ".replay-tmp" already exists, ReplayFiles returns an os.ErrExist
// error and leaves that pre-existing file untouched; the temporary file is
// removed on error only when this call created it.
func ReplayFiles(basePath, walPath, outPath string) (ReplayReport, error) {
	for _, path := range []string{outPath, outPath + "-wal", outPath + "-journal", outPath + "-shm"} {
		if _, err := os.Lstat(path); err == nil {
			return ReplayReport{}, fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(path))
		}
	}
	base, err := os.ReadFile(basePath)
	if err != nil {
		return ReplayReport{}, err
	}
	wal, err := os.ReadFile(walPath)
	if errors.Is(err, os.ErrNotExist) {
		wal = nil
	} else if err != nil {
		return ReplayReport{}, err
	}
	out, report, err := Replay(base, wal)
	if err != nil {
		return report, err
	}
	temp := outPath + ".replay-tmp"
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return report, err
	}
	if err := writeSynced(f, out); err != nil {
		os.Remove(temp)
		return report, err
	}
	if err := os.Rename(temp, outPath); err != nil {
		os.Remove(temp)
		return report, err
	}
	return report, nil
}

func writeSynced(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
