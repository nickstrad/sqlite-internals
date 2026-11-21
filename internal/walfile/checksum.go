package walfile

import (
	"encoding/binary"
	"fmt"
)

// Checksum holds SQLite's two-word WAL cumulative checksum (s1, s2).
type Checksum [2]uint32

// String formats both checksum words as 8-digit lowercase hex.
func (c Checksum) String() string {
	return fmt.Sprintf("%08x %08x", c[0], c[1])
}

// ChecksumBytes extends seed over data using SQLite's WAL Fletcher-like
// checksum (src/wal.c:856-912, walChecksumBytes). Input length must be a
// positive multiple of 8 bytes because the loop consumes 32-bit word pairs.
func ChecksumBytes(bigEndianWords bool, data []byte, seed Checksum) (Checksum, error) {
	if len(data) == 0 || len(data)%8 != 0 {
		return seed, fmt.Errorf("checksum input of %d bytes is not a positive multiple of 8", len(data))
	}
	return sumWordPairs(bigEndianWords, data, seed), nil
}

// sumWordPairs implements the inner loop of walChecksumBytes (src/wal.c:877-911).
// Each pair of 32-bit words is read in the byte order selected by the WAL magic
// bit (big-endian for 0x377f0683, little-endian for 0x377f0682), while the
// resulting s1 and s2 values are always stored big-endian on disk.
func sumWordPairs(bigEndianWords bool, data []byte, seed Checksum) Checksum {
	var order binary.ByteOrder = binary.LittleEndian
	if bigEndianWords {
		order = binary.BigEndian
	}
	s1, s2 := seed[0], seed[1]
	for i := 0; i+8 <= len(data); i += 8 {
		s1 += order.Uint32(data[i:]) + s2
		s2 += order.Uint32(data[i+4:]) + s1
	}
	return Checksum{s1, s2}
}
