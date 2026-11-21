package walship

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

// ErrChecksumChain reports that a segment's raw WAL frames do not match its
// generation salts or do not continue SQLite's cumulative WAL checksum chain
// from SeedChecksum to LastChecksum.
var ErrChecksumChain = errors.New("segment frames do not continue the WAL checksum chain")

// VerifySegmentFrames validates a segment before any page is replayed: it parses
// the 32-byte WAL generation header (magic, format version 3007000, page size,
// header checksum), matches every frame's salts and page number, verifies a
// one-to-one match between non-zero frame commit sizes and meta.Commits, and
// walks SQLite's cumulative checksum chain (walDecodeFrame, src/wal.c:1390-1614)
// from meta.SeedChecksum to meta.LastChecksum.
func VerifySegmentFrames(meta SegmentMeta, frames []byte) error {
	header, err := meta.Generation.Bytes()
	if err != nil {
		return err
	}
	parsed, err := walfile.Parse(header)
	if err != nil {
		return fmt.Errorf("%w: segment WAL header: %v", ErrMalformedObject, err)
	}
	if parsed.PageSize() != meta.PageSize {
		return fmt.Errorf("%w: segment WAL header page size %d, metadata page size %d", ErrMalformedObject, parsed.PageSize(), meta.PageSize)
	}
	bigEndian := parsed.Header.BigEndianChecksums()
	frameSize := meta.PageSize + walfile.FrameHeaderSize
	if len(frames) != (meta.LastFrame-meta.FirstFrame+1)*frameSize {
		return fmt.Errorf("%w: %d bytes for frames %d-%d", ErrMalformedObject, len(frames), meta.FirstFrame, meta.LastFrame)
	}
	chain := walfile.Checksum(meta.SeedChecksum)
	commit := 0
	for offset, number := 0, meta.FirstFrame; offset < len(frames); offset, number = offset+frameSize, number+1 {
		frame := frames[offset : offset+frameSize]
		if string(frame[8:16]) != string(header[16:24]) {
			return fmt.Errorf("%w: frame %d carries other salts than its generation", ErrChecksumChain, number)
		}
		if binary.BigEndian.Uint32(frame[0:]) == 0 {
			return fmt.Errorf("%w: frame %d has page number 0", ErrChecksumChain, number)
		}
		listed := commit < len(meta.Commits) && meta.Commits[commit].Frame == number
		// Require every frame with a non-zero commit-size field (frame header
		// bytes 4-7) to match the next listed CommitPoint, and forbid listing a
		// non-commit frame as a restore point.
		if commitSize := binary.BigEndian.Uint32(frame[4:]); commitSize != 0 || listed {
			if !listed || meta.Commits[commit].Pages != commitSize {
				return fmt.Errorf("%w: frame %d has commit size %d, which the segment's commit points do not list", ErrMalformedObject, number, commitSize)
			}
			commit++
		}
		next, err := walfile.ChecksumBytes(bigEndian, frame[:8], chain)
		if err != nil {
			return err
		}
		next, err = walfile.ChecksumBytes(bigEndian, frame[walfile.FrameHeaderSize:], next)
		if err != nil {
			return err
		}
		stored := walfile.Checksum{binary.BigEndian.Uint32(frame[16:]), binary.BigEndian.Uint32(frame[20:])}
		if next != stored {
			return fmt.Errorf("%w: frame %d stores a checksum the chain does not give", ErrChecksumChain, number)
		}
		chain = next
	}
	if commit != len(meta.Commits) {
		return fmt.Errorf("%w: %d commit points, %d commit frames", ErrMalformedObject, len(meta.Commits), commit)
	}
	if chain != walfile.Checksum(meta.LastChecksum) {
		return fmt.Errorf("%w: the chain does not end at the checksum the metadata records", ErrChecksumChain)
	}
	return nil
}

// StandaloneWAL packages frames [meta.FirstFrame..throughFrame] with the
// segment's 32-byte WAL header into a self-contained WAL file whose cumulative
// frame checksums are re-chained starting from the header checksum. Re-chaining
// is required because a segment starting at FirstFrame > 1 originally chained
// from an earlier frame's checksum and would not validate on its own in
// walfile.Parse or walfile.Replay.
func StandaloneWAL(meta SegmentMeta, frames []byte, throughFrame int) ([]byte, error) {
	if throughFrame < meta.FirstFrame || throughFrame > meta.LastFrame {
		return nil, fmt.Errorf("frame %d is outside the segment's frames %d-%d", throughFrame, meta.FirstFrame, meta.LastFrame)
	}
	header, err := meta.Generation.Bytes()
	if err != nil {
		return nil, err
	}
	bigEndian := binary.BigEndian.Uint32(header[0:])&1 == 1
	frameSize := meta.PageSize + walfile.FrameHeaderSize
	count := throughFrame - meta.FirstFrame + 1
	if len(frames) < count*frameSize {
		return nil, fmt.Errorf("%w: segment body too short for frame %d", ErrMalformedObject, throughFrame)
	}
	out := make([]byte, walfile.HeaderSize+count*frameSize)
	copy(out, header)
	copy(out[walfile.HeaderSize:], frames[:count*frameSize])
	chain := walfile.Checksum{binary.BigEndian.Uint32(header[24:]), binary.BigEndian.Uint32(header[28:])}
	for i := 0; i < count; i++ {
		frame := out[walfile.HeaderSize+i*frameSize : walfile.HeaderSize+(i+1)*frameSize]
		next, err := walfile.ChecksumBytes(bigEndian, frame[:8], chain)
		if err != nil {
			return nil, err
		}
		next, err = walfile.ChecksumBytes(bigEndian, frame[walfile.FrameHeaderSize:], next)
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(frame[16:], next[0])
		binary.BigEndian.PutUint32(frame[20:], next[1])
		chain = next
	}
	return out, nil
}

// ApplySegment verifies meta and frames, builds a standalone WAL through
// throughFrame, and replays it onto image using Chapter 5's walfile.Replay.
// It fails unless every frame in [meta.FirstFrame..throughFrame] is valid and
// throughFrame is a commit frame.
func ApplySegment(image []byte, meta SegmentMeta, frames []byte, throughFrame int) ([]byte, walfile.ReplayReport, error) {
	if err := VerifySegmentFrames(meta, frames); err != nil {
		return nil, walfile.ReplayReport{}, err
	}
	wal, err := StandaloneWAL(meta, frames, throughFrame)
	if err != nil {
		return nil, walfile.ReplayReport{}, err
	}
	out, report, err := walfile.Replay(image, wal)
	if err != nil {
		return nil, report, err
	}
	want := throughFrame - meta.FirstFrame + 1
	if report.ValidFrames != want || report.LastCommitFrame != want {
		return nil, report, fmt.Errorf("replay of frames %d-%d found %d valid frames, last commit at %d; want %d and %d", meta.FirstFrame, throughFrame, report.ValidFrames, report.LastCommitFrame, want, want)
	}
	return out, report, nil
}
