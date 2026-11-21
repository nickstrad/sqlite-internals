package walship

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/walfile"
)

const (
	SnapshotPrefix = "snapshots/"
	SegmentPrefix  = "segments/"
	positionDigits = 12
	snapshotMagic  = "walship snapshot v1"
	segmentMagic   = "walship segment v1"
)

const (
	ReasonInitial       = "initial"
	ReasonPeriodic      = "periodic"
	ReasonDiscontinuity = "discontinuity"
)

var ErrMalformedObject = errors.New("malformed walship object")

type Generation struct {
	Header string `json:"header"`
}

func GenerationOf(header []byte) Generation {
	return Generation{Header: hex.EncodeToString(header)}
}

func (g Generation) None() bool { return g.Header == "" }

func (g Generation) Bytes() ([]byte, error) {
	raw, err := hex.DecodeString(g.Header)
	if err != nil {
		return nil, err
	}
	if len(raw) != walfile.HeaderSize {
		return nil, fmt.Errorf("generation header is %d bytes, want %d", len(raw), walfile.HeaderSize)
	}
	return raw, nil
}

func (g Generation) Salts() (uint32, uint32) {
	raw, err := g.Bytes()
	if err != nil {
		return 0, 0
	}
	return binary.BigEndian.Uint32(raw[16:]), binary.BigEndian.Uint32(raw[20:])
}

func (g Generation) String() string {
	if g.None() {
		return "no WAL header"
	}
	raw, err := g.Bytes()
	if err != nil {
		return "bad header"
	}
	salt1, salt2 := g.Salts()
	return fmt.Sprintf("checkpoint-seq=%d salt-1=0x%08x salt-2=0x%08x", binary.BigEndian.Uint32(raw[12:]), salt1, salt2)
}

type WALPosition struct {
	Generation Generation `json:"generation"`
	Frame      int        `json:"frame"`
	Checksum   [2]uint32  `json:"checksum"`
}

type CommitPoint struct {
	Position    int64  `json:"position"`
	Frame       int    `json:"frame"`
	Pages       uint32 `json:"pages"`
	UserVersion uint32 `json:"user_version"`
	StateSHA256 string `json:"state_sha256"`
}

type SnapshotMeta struct {
	Position    int64       `json:"position"`
	Reason      string      `json:"reason"`
	PageSize    int         `json:"page_size"`
	Pages       uint32      `json:"pages"`
	UserVersion uint32      `json:"user_version"`
	StateSHA256 string      `json:"state_sha256"`
	WAL         WALPosition `json:"wal"`
	BodyBytes   int64       `json:"body_bytes"`
	BodySHA256  string      `json:"body_sha256"`
}

type SegmentMeta struct {
	FirstPosition int64         `json:"first_position"`
	LastPosition  int64         `json:"last_position"`
	Generation    Generation    `json:"generation"`
	FirstFrame    int           `json:"first_frame"`
	LastFrame     int           `json:"last_frame"`
	SeedChecksum  [2]uint32     `json:"seed_checksum"`
	LastChecksum  [2]uint32     `json:"last_checksum"`
	PageSize      int           `json:"page_size"`
	BeforeSHA256  string        `json:"before_sha256"`
	AfterSHA256   string        `json:"after_sha256"`
	Commits       []CommitPoint `json:"commits"`
	BodyBytes     int64         `json:"body_bytes"`
	BodySHA256    string        `json:"body_sha256"`
}

func (m SegmentMeta) End() WALPosition {
	return WALPosition{Generation: m.Generation, Frame: m.LastFrame, Checksum: m.LastChecksum}
}

func (m SegmentMeta) Commit(position int64) (CommitPoint, bool) {
	for _, c := range m.Commits {
		if c.Position == position {
			return c, true
		}
	}
	return CommitPoint{}, false
}

func SnapshotKey(position int64) string { return SnapshotPrefix + formatPosition(position) }

func SegmentKey(firstPosition int64) string { return SegmentPrefix + formatPosition(firstPosition) }

func formatPosition(position int64) string {
	return fmt.Sprintf("%0*d", positionDigits, position)
}

func ParseKey(key string) (kind string, position int64, err error) {
	var rest string
	switch {
	case strings.HasPrefix(key, SnapshotPrefix):
		kind, rest = "snapshot", strings.TrimPrefix(key, SnapshotPrefix)
	case strings.HasPrefix(key, SegmentPrefix):
		kind, rest = "segment", strings.TrimPrefix(key, SegmentPrefix)
	default:
		return "", 0, fmt.Errorf("%w: key %q is neither a snapshot nor a segment", ErrMalformedObject, key)
	}
	if len(rest) != positionDigits {
		return "", 0, fmt.Errorf("%w: key %q: position must be %d digits", ErrMalformedObject, key, positionDigits)
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return "", 0, fmt.Errorf("%w: key %q: position must be decimal digits", ErrMalformedObject, key)
		}
	}
	position, err = strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: key %q: %v", ErrMalformedObject, key, err)
	}
	return kind, position, nil
}

func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encodeObject(magic string, meta any, body []byte) ([]byte, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString(magic)
	out.WriteByte('\n')
	out.Write(encoded)
	out.WriteByte('\n')
	out.Write(body)
	return out.Bytes(), nil
}

func decodeObject(magic string, data []byte, meta any) ([]byte, error) {
	first := bytes.IndexByte(data, '\n')
	if first < 0 || string(data[:first]) != magic {
		return nil, fmt.Errorf("%w: first line is not %q", ErrMalformedObject, magic)
	}
	rest := data[first+1:]
	second := bytes.IndexByte(rest, '\n')
	if second < 0 {
		return nil, fmt.Errorf("%w: no metadata line", ErrMalformedObject)
	}
	decoder := json.NewDecoder(bytes.NewReader(rest[:second]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(meta); err != nil {
		return nil, fmt.Errorf("%w: metadata: %v", ErrMalformedObject, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: metadata line holds more than one value", ErrMalformedObject)
	}
	return rest[second+1:], nil
}

func checkBody(body []byte, wantBytes int64, wantSHA256 string) error {
	if int64(len(body)) != wantBytes {
		return fmt.Errorf("%w: body is %d bytes, metadata says %d", ErrMalformedObject, len(body), wantBytes)
	}
	if SHA256Hex(body) != wantSHA256 {
		return fmt.Errorf("%w: body sha256 does not match its metadata", ErrMalformedObject)
	}
	return nil
}

func EncodeSnapshot(meta SnapshotMeta, image []byte) ([]byte, error) {
	meta.BodyBytes = int64(len(image))
	meta.BodySHA256 = SHA256Hex(image)
	return encodeObject(snapshotMagic, meta, image)
}

func DecodeSnapshot(data []byte) (SnapshotMeta, []byte, error) {
	var meta SnapshotMeta
	body, err := decodeObject(snapshotMagic, data, &meta)
	if err != nil {
		return meta, nil, err
	}
	if err := checkBody(body, meta.BodyBytes, meta.BodySHA256); err != nil {
		return meta, nil, err
	}
	if meta.PageSize < walfile.MinPageSize || meta.PageSize > walfile.MaxPageSize || meta.PageSize&(meta.PageSize-1) != 0 {
		return meta, nil, fmt.Errorf("%w: snapshot page size %d", ErrMalformedObject, meta.PageSize)
	}
	if !meta.WAL.Generation.None() {
		if _, err := meta.WAL.Generation.Bytes(); err != nil {
			return meta, nil, fmt.Errorf("%w: snapshot WAL generation: %v", ErrMalformedObject, err)
		}
	}
	if meta.Pages == 0 || len(body)%meta.PageSize != 0 || uint32(len(body)/meta.PageSize) != meta.Pages {
		return meta, nil, fmt.Errorf("%w: snapshot body of %d bytes is not %d pages of %d bytes", ErrMalformedObject, len(body), meta.Pages, meta.PageSize)
	}
	if SHA256Hex(body) != meta.StateSHA256 {
		return meta, nil, fmt.Errorf("%w: snapshot state hash does not match its image", ErrMalformedObject)
	}
	return meta, body, nil
}

func EncodeSegment(meta SegmentMeta, frames []byte) ([]byte, error) {
	meta.BodyBytes = int64(len(frames))
	meta.BodySHA256 = SHA256Hex(frames)
	return encodeObject(segmentMagic, meta, frames)
}

func DecodeSegment(data []byte) (SegmentMeta, []byte, error) {
	var meta SegmentMeta
	body, err := decodeObject(segmentMagic, data, &meta)
	if err != nil {
		return meta, nil, err
	}
	if err := checkBody(body, meta.BodyBytes, meta.BodySHA256); err != nil {
		return meta, nil, err
	}
	if err := meta.validate(len(body)); err != nil {
		return meta, nil, err
	}
	return meta, body, nil
}

func (m SegmentMeta) validate(bodyBytes int) error {
	if _, err := m.Generation.Bytes(); err != nil {
		return fmt.Errorf("%w: segment generation: %v", ErrMalformedObject, err)
	}
	if m.PageSize < walfile.MinPageSize || m.PageSize > walfile.MaxPageSize {
		return fmt.Errorf("%w: page size %d", ErrMalformedObject, m.PageSize)
	}
	if m.FirstFrame < 1 || m.LastFrame < m.FirstFrame {
		return fmt.Errorf("%w: frames %d-%d", ErrMalformedObject, m.FirstFrame, m.LastFrame)
	}
	frameSize := m.PageSize + walfile.FrameHeaderSize
	if bodyBytes != (m.LastFrame-m.FirstFrame+1)*frameSize {
		return fmt.Errorf("%w: body of %d bytes does not hold frames %d-%d", ErrMalformedObject, bodyBytes, m.FirstFrame, m.LastFrame)
	}
	if len(m.Commits) == 0 || m.LastPosition-m.FirstPosition+1 != int64(len(m.Commits)) {
		return fmt.Errorf("%w: positions %d-%d with %d commit points", ErrMalformedObject, m.FirstPosition, m.LastPosition, len(m.Commits))
	}
	previousFrame := m.FirstFrame - 1
	for i, c := range m.Commits {
		if c.Position != m.FirstPosition+int64(i) {
			return fmt.Errorf("%w: commit point %d has position %d", ErrMalformedObject, i, c.Position)
		}
		if c.Frame <= previousFrame || c.Frame > m.LastFrame || c.Pages == 0 {
			return fmt.Errorf("%w: commit point at position %d has frame %d, pages %d", ErrMalformedObject, c.Position, c.Frame, c.Pages)
		}
		previousFrame = c.Frame
	}
	if previousFrame != m.LastFrame {
		return fmt.Errorf("%w: the last frame %d is not a commit frame", ErrMalformedObject, m.LastFrame)
	}
	if m.Commits[len(m.Commits)-1].StateSHA256 != m.AfterSHA256 {
		return fmt.Errorf("%w: after hash differs from the last commit point", ErrMalformedObject)
	}
	return nil
}

func short(digest string) string {
	if len(digest) > 16 {
		return digest[:16]
	}
	return digest
}

func userVersion(image []byte) uint32 {
	if len(image) < 64 {
		return 0
	}
	return binary.BigEndian.Uint32(image[60:64])
}
