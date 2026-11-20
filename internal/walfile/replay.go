package walfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var databaseMagic = []byte("SQLite format 3\x00")

type IgnoredFrames struct {
	FirstFrame int
	LastFrame  int
	Reason     string
}

func (g IgnoredFrames) Count() int { return g.LastFrame - g.FirstFrame + 1 }

func (g IgnoredFrames) String() string {
	if g.FirstFrame == g.LastFrame {
		return fmt.Sprintf("frame %d: %s", g.FirstFrame, g.Reason)
	}
	return fmt.Sprintf("frames %d-%d (%d): %s", g.FirstFrame, g.LastFrame, g.Count(), g.Reason)
}

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

func (r ReplayReport) IgnoredFrameCount() int {
	total := 0
	for _, g := range r.Ignored {
		total += g.Count()
	}
	return total
}

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
	size := int(base[16])<<8 | int(base[17])
	if size == 1 {
		size = 65536
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
	if file.LastCommitFrame == 0 {
		report.OutputBytes = int64(len(base))
		return bytes.Clone(base), report, nil
	}
	walPageSize := file.PageSize()
	if pageSize != 0 && pageSize != walPageSize {
		return nil, report, fmt.Errorf("%w: base %d, WAL %d", ErrPageSizeMismatch, pageSize, walPageSize)
	}
	if len(base)%walPageSize != 0 {
		return nil, report, fmt.Errorf("%w: %d bytes, page size %d", ErrBaseSize, len(base), walPageSize)
	}
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
	if err := writeSynced(temp, out); err != nil {
		os.Remove(temp)
		return report, err
	}
	if err := os.Rename(temp, outPath); err != nil {
		os.Remove(temp)
		return report, err
	}
	return report, nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
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
