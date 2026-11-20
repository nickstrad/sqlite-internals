package walfile

import (
	"encoding/binary"
	"fmt"
)

type Checksum [2]uint32

func (c Checksum) String() string {
	return fmt.Sprintf("%08x %08x", c[0], c[1])
}

func ChecksumBytes(bigEndianWords bool, data []byte, seed Checksum) (Checksum, error) {
	if len(data) == 0 || len(data)%8 != 0 {
		return seed, fmt.Errorf("checksum input of %d bytes is not a positive multiple of 8", len(data))
	}
	return sumWordPairs(bigEndianWords, data, seed), nil
}

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
