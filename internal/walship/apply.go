package walship

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

var ErrChecksumChain = errors.New("segment frames do not continue the WAL checksum chain")

func VerifySegmentFrames(meta SegmentMeta, frames []byte) error {
	header, err := meta.Generation.Bytes()
	if err != nil {
		return err
	}
	bigEndian := binary.BigEndian.Uint32(header[0:])&1 == 1
	frameSize := meta.PageSize + walfile.FrameHeaderSize
	if len(frames) != (meta.LastFrame-meta.FirstFrame+1)*frameSize {
		return fmt.Errorf("%w: %d bytes for frames %d-%d", ErrMalformedObject, len(frames), meta.FirstFrame, meta.LastFrame)
	}
	chain := walfile.Checksum(meta.SeedChecksum)
	for offset, number := 0, meta.FirstFrame; offset < len(frames); offset, number = offset+frameSize, number+1 {
		frame := frames[offset : offset+frameSize]
		if string(frame[8:16]) != string(header[16:24]) {
			return fmt.Errorf("%w: frame %d carries other salts than its generation", ErrChecksumChain, number)
		}
		if binary.BigEndian.Uint32(frame[0:]) == 0 {
			return fmt.Errorf("%w: frame %d has page number 0", ErrChecksumChain, number)
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
	if chain != walfile.Checksum(meta.LastChecksum) {
		return fmt.Errorf("%w: the chain does not end at the checksum the metadata records", ErrChecksumChain)
	}
	return nil
}

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
