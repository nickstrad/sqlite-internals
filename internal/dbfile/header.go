package dbfile

import (
	"encoding/binary"
	"fmt"
)

const (
	HeaderSize         = 100
	MagicString        = "SQLite format 3\x00"
	LockByteOffset     = 1073741824
	MinPageSize        = 512
	MaxPageSize        = 65536
	MinUsableSize      = 480
	TextEncodingUTF8   = 1
	TextEncodingUTF16L = 2
	TextEncodingUTF16B = 3
)

type Header struct {
	Raw                 [HeaderSize]byte
	PageSize            int
	WriteVersion        int
	ReadVersion         int
	ReservedBytes       int
	MaxPayloadFraction  int
	MinPayloadFraction  int
	LeafPayloadFraction int
	ChangeCounter       uint32
	DatabaseSize        uint32
	FirstFreelistTrunk  uint32
	FreelistCount       uint32
	SchemaCookie        uint32
	SchemaFormat        uint32
	DefaultCacheSize    int32
	LargestRootPage     uint32
	TextEncoding        uint32
	UserVersion         int32
	IncrementalVacuum   uint32
	ApplicationID       int32
	VersionValidFor     uint32
	SQLiteVersion       uint32
}

func be16(b []byte) int     { return int(binary.BigEndian.Uint16(b)) }
func be32(b []byte) uint32  { return binary.BigEndian.Uint32(b) }
func be32i(b []byte) int32  { return int32(binary.BigEndian.Uint32(b)) }
func be32int(b []byte) int  { return int(binary.BigEndian.Uint32(b)) }
func be64i(b []byte) int64  { return int64(binary.BigEndian.Uint64(b)) }
func be64u(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

func DecodePageSize(stored int) int {
	if stored == 1 {
		return MaxPageSize
	}
	return stored
}

func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderSize {
		return h, &FormatError{Kind: ErrTruncated, Offset: len(b), Problem: fmt.Sprintf("header needs %d bytes, have %d", HeaderSize, len(b))}
	}
	copy(h.Raw[:], b[:HeaderSize])
	if string(b[:16]) != MagicString {
		return h, &FormatError{Kind: ErrNotADatabase, Offset: 0, Problem: fmt.Sprintf("magic is %q", b[:16])}
	}
	h.PageSize = DecodePageSize(be16(b[16:18]))
	h.WriteVersion = int(b[18])
	h.ReadVersion = int(b[19])
	h.ReservedBytes = int(b[20])
	h.MaxPayloadFraction = int(b[21])
	h.MinPayloadFraction = int(b[22])
	h.LeafPayloadFraction = int(b[23])
	h.ChangeCounter = be32(b[24:28])
	h.DatabaseSize = be32(b[28:32])
	h.FirstFreelistTrunk = be32(b[32:36])
	h.FreelistCount = be32(b[36:40])
	h.SchemaCookie = be32(b[40:44])
	h.SchemaFormat = be32(b[44:48])
	h.DefaultCacheSize = be32i(b[48:52])
	h.LargestRootPage = be32(b[52:56])
	h.TextEncoding = be32(b[56:60])
	h.UserVersion = be32i(b[60:64])
	h.IncrementalVacuum = be32(b[64:68])
	h.ApplicationID = be32i(b[68:72])
	h.VersionValidFor = be32(b[92:96])
	h.SQLiteVersion = be32(b[96:100])
	if err := h.validate(); err != nil {
		return h, err
	}
	return h, nil
}

func (h Header) validate() error {
	bad := func(offset int, format string, args ...any) error {
		return &FormatError{Kind: ErrNotADatabase, Offset: offset, Problem: fmt.Sprintf(format, args...)}
	}
	if h.PageSize < MinPageSize || h.PageSize > MaxPageSize || h.PageSize&(h.PageSize-1) != 0 {
		return bad(16, "page size %d is not a power of two between %d and %d", h.PageSize, MinPageSize, MaxPageSize)
	}
	if h.ReadVersion != 1 && h.ReadVersion != 2 {
		return bad(19, "read version %d is not 1 (rollback) or 2 (WAL)", h.ReadVersion)
	}
	if h.WriteVersion != 1 && h.WriteVersion != 2 {
		return bad(18, "write version %d is not 1 (rollback) or 2 (WAL)", h.WriteVersion)
	}
	if h.MaxPayloadFraction != 64 || h.MinPayloadFraction != 32 || h.LeafPayloadFraction != 32 {
		return bad(21, "payload fractions %d/%d/%d are not 64/32/32", h.MaxPayloadFraction, h.MinPayloadFraction, h.LeafPayloadFraction)
	}
	if h.UsableSize() < MinUsableSize {
		return bad(20, "usable size %d (page size %d minus %d reserved) is below %d", h.UsableSize(), h.PageSize, h.ReservedBytes, MinUsableSize)
	}
	if h.TextEncoding > TextEncodingUTF16B {
		return bad(56, "text encoding %d is not 1, 2 or 3", h.TextEncoding)
	}
	if h.SchemaFormat > 4 {
		return bad(44, "schema format %d is above 4", h.SchemaFormat)
	}
	return nil
}

func (h Header) UsableSize() int { return h.PageSize - h.ReservedBytes }

func (h Header) ChangeCountersAgree() bool { return h.ChangeCounter == h.VersionValidFor }

func (h Header) InHeaderSizeValid() bool {
	return h.DatabaseSize != 0 && h.ChangeCountersAgree()
}

func (h Header) WALMode() bool { return h.ReadVersion == 2 && h.WriteVersion == 2 }

func (h Header) AutoVacuum() bool { return h.LargestRootPage != 0 }

func (h Header) LockBytePage() int { return LockByteOffset/h.PageSize + 1 }

func (h Header) EncodingName() string {
	switch h.TextEncoding {
	case 0:
		return "unset"
	case TextEncodingUTF8:
		return "UTF-8"
	case TextEncodingUTF16L:
		return "UTF-16le"
	case TextEncodingUTF16B:
		return "UTF-16be"
	}
	return fmt.Sprintf("unknown(%d)", h.TextEncoding)
}

func (h Header) AutoVacuumName() string {
	switch {
	case h.LargestRootPage == 0:
		return "NONE"
	case h.IncrementalVacuum != 0:
		return "INCREMENTAL"
	}
	return "FULL"
}

func FormatSQLiteVersion(v uint32) string {
	return fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000)
}

type HeaderField struct {
	Offset int
	Size   int
	Name   string
	Value  string
}

func (h Header) Fields() []HeaderField {
	u := func(v uint32) string { return fmt.Sprint(v) }
	i := func(v int32) string { return fmt.Sprint(v) }
	stored := be16(h.Raw[16:18])
	pageSize := fmt.Sprint(h.PageSize)
	if stored != h.PageSize {
		pageSize = fmt.Sprintf("%d (stored as %d)", h.PageSize, stored)
	}
	return []HeaderField{
		{0, 16, "magic", fmt.Sprintf("%q", string(h.Raw[:16]))},
		{16, 2, "page size", pageSize},
		{18, 1, "write version", fmt.Sprint(h.WriteVersion)},
		{19, 1, "read version", fmt.Sprint(h.ReadVersion)},
		{20, 1, "reserved bytes per page", fmt.Sprint(h.ReservedBytes)},
		{21, 1, "max embedded payload fraction", fmt.Sprint(h.MaxPayloadFraction)},
		{22, 1, "min embedded payload fraction", fmt.Sprint(h.MinPayloadFraction)},
		{23, 1, "leaf payload fraction", fmt.Sprint(h.LeafPayloadFraction)},
		{24, 4, "file change counter", u(h.ChangeCounter)},
		{28, 4, "database size in pages", u(h.DatabaseSize)},
		{32, 4, "first freelist trunk page", u(h.FirstFreelistTrunk)},
		{36, 4, "freelist page count", u(h.FreelistCount)},
		{40, 4, "schema cookie", u(h.SchemaCookie)},
		{44, 4, "schema format", u(h.SchemaFormat)},
		{48, 4, "default cache size", i(h.DefaultCacheSize)},
		{52, 4, "largest root page (auto-vacuum)", u(h.LargestRootPage)},
		{56, 4, "text encoding", fmt.Sprintf("%d (%s)", h.TextEncoding, h.EncodingName())},
		{60, 4, "user version", i(h.UserVersion)},
		{64, 4, "incremental vacuum", u(h.IncrementalVacuum)},
		{68, 4, "application id", i(h.ApplicationID)},
		{72, 20, "reserved for expansion", reservedText(h.Raw[72:92])},
		{92, 4, "version-valid-for", u(h.VersionValidFor)},
		{96, 4, "SQLite version number", fmt.Sprintf("%d (%s)", h.SQLiteVersion, FormatSQLiteVersion(h.SQLiteVersion))},
	}
}

func reservedText(b []byte) string {
	for _, c := range b {
		if c != 0 {
			return fmt.Sprintf("%x", b)
		}
	}
	return "all zero"
}

func HeaderFieldAt(offset int) (HeaderField, bool) {
	for _, f := range (Header{}).Fields() {
		if offset >= f.Offset && offset < f.Offset+f.Size {
			return HeaderField{Offset: f.Offset, Size: f.Size, Name: f.Name}, true
		}
	}
	return HeaderField{}, false
}
