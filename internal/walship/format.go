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

// Object key prefixes and wire-format constants. Every snapshot and segment is
// stored as a single self-contained object (magic line, JSON metadata line,
// raw body) so one atomic PutIfAbsent publishes both metadata and payload.
const (
	// SnapshotPrefix is the key prefix for full-database snapshot objects.
	SnapshotPrefix = "snapshots/"
	// SegmentPrefix is the key prefix for WAL frame segment objects.
	SegmentPrefix  = "segments/"
	positionDigits = 12
	snapshotMagic  = "walship snapshot v1"
	segmentMagic   = "walship segment v1"
)

// Reasons recorded in SnapshotMeta.Reason explaining why a snapshot was taken.
const (
	// ReasonInitial marks the base snapshot taken when opening an empty store.
	ReasonInitial = "initial"
	// ReasonPeriodic marks a checkpointed snapshot taken every SnapshotEvery segments.
	ReasonPeriodic = "periodic"
	// ReasonDiscontinuity marks a re-base snapshot taken after an unvouched WAL change.
	ReasonDiscontinuity = "discontinuity"
)

// ErrMalformedObject reports a snapshot or segment object whose magic header,
// JSON metadata, key naming, body length/hash, or frame layout is invalid.
var ErrMalformedObject = errors.New("malformed walship object")

// Generation identifies one WAL generation by the hex encoding of its 32-byte
// WAL header (including its magic, checkpoint sequence, salts, and checksum).
// An empty Header means no WAL file (or a zero-byte WAL) existed.
type Generation struct {
	Header string `json:"header"`
}

// GenerationOf returns the Generation encoded from a 32-byte WAL header slice.
func GenerationOf(header []byte) Generation {
	return Generation{Header: hex.EncodeToString(header)}
}

// None reports whether g represents the absence of a WAL header.
func (g Generation) None() bool { return g.Header == "" }

// Bytes decodes g.Header and verifies that it is exactly walfile.HeaderSize (32) bytes.
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

// Salts returns salt-1 and salt-2 from bytes 16-23 of the WAL header, or (0, 0)
// if g is empty or invalid.
func (g Generation) Salts() (uint32, uint32) {
	raw, err := g.Bytes()
	if err != nil {
		return 0, 0
	}
	return binary.BigEndian.Uint32(raw[16:]), binary.BigEndian.Uint32(raw[20:])
}

// String formats g's checkpoint sequence and random salts for diagnostics.
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

// WALPosition records the exact cursor inside a WAL generation—header, 1-based
// frame number (0 for the header itself), and cumulative checksum at that frame—
// so the next segment can prove WAL continuity.
type WALPosition struct {
	Generation Generation `json:"generation"`
	Frame      int        `json:"frame"`
	Checksum   [2]uint32  `json:"checksum"`
}

// CommitPoint describes one committed transaction inside a SegmentMeta: its
// stream position, 1-based WAL commit frame, committed database size in pages,
// header user_version, and the SHA-256 of the complete checkpointed database
// image at that commit.
type CommitPoint struct {
	Position    int64  `json:"position"`
	Frame       int    `json:"frame"`
	Pages       uint32 `json:"pages"`
	UserVersion uint32 `json:"user_version"`
	StateSHA256 string `json:"state_sha256"`
}

// SnapshotMeta is the JSON metadata header stored ahead of a full raw database
// image in a snapshot object.
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

// SegmentMeta is the JSON metadata header stored ahead of raw WAL frames
// [FirstFrame..LastFrame] in a segment object. BeforeSHA256 and AfterSHA256
// chain consecutive segments (and their base snapshot) by full-image SHA-256.
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

// End returns the WALPosition at the final frame of m, used to check that the
// following segment continues cleanly.
func (m SegmentMeta) End() WALPosition {
	return WALPosition{Generation: m.Generation, Frame: m.LastFrame, Checksum: m.LastChecksum}
}

// Commit looks up the CommitPoint for stream position within m.
func (m SegmentMeta) Commit(position int64) (CommitPoint, bool) {
	for _, c := range m.Commits {
		if c.Position == position {
			return c, true
		}
	}
	return CommitPoint{}, false
}

// SnapshotKey returns the fixed-width key for the snapshot at position.
func SnapshotKey(position int64) string { return SnapshotPrefix + formatPosition(position) }

// SegmentKey returns the fixed-width key for the segment starting at firstPosition.
func SegmentKey(firstPosition int64) string { return SegmentPrefix + formatPosition(firstPosition) }

func formatPosition(position int64) string {
	return fmt.Sprintf("%0*d", positionDigits, position)
}

// ParseKey splits a snapshot or segment object key into its kind ("snapshot"
// or "segment") and 12-digit decimal position.
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

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of data.
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
	// Reject unknown JSON fields and trailing JSON values so metadata lines
	// cannot smuggle extra state past validation.
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

// EncodeSnapshot populates BodyBytes and BodySHA256 on meta and serializes the
// snapshot header and raw database image into one object.
func EncodeSnapshot(meta SnapshotMeta, image []byte) ([]byte, error) {
	meta.BodyBytes = int64(len(image))
	meta.BodySHA256 = SHA256Hex(image)
	return encodeObject(snapshotMagic, meta, image)
}

// DecodeSnapshot parses and validates a snapshot object, checking its magic
// line, JSON metadata, body length and SHA-256, power-of-two page size, page
// count, WAL generation header length, and StateSHA256.
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

// EncodeSegment populates BodyBytes and BodySHA256 on meta and serializes the
// segment header and raw WAL frame bytes into one object.
func EncodeSegment(meta SegmentMeta, frames []byte) ([]byte, error) {
	meta.BodyBytes = int64(len(frames))
	meta.BodySHA256 = SHA256Hex(frames)
	return encodeObject(segmentMagic, meta, frames)
}

// DecodeSegment parses and validates a segment object's wire format, body hash,
// and metadata invariants (frame count, consecutive commit positions, and final
// commit hash). Call VerifySegmentFrames as well to check WAL header validity,
// commit-frame alignment, and SQLite's cumulative checksum chain.
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

// userVersion reads SQLite's PRAGMA user_version from database header bytes
// 60-63 of page 1.
func userVersion(image []byte) uint32 {
	if len(image) < 64 {
		return 0
	}
	return binary.BigEndian.Uint32(image[60:64])
}
