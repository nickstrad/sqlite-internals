package dbfile

import "errors"

var errVarintTruncated = errors.New("varint runs past the end of its buffer")

// MaxVarintLength is the maximum byte length of an SQLite variable-length
// integer (src/btreeInt.h:170-176, src/util.c:1633-1781).
const MaxVarintLength = 9

// Varint decodes one big-endian 1-to-9-byte SQLite varint from b, returning its
// unsigned 64-bit value and byte length. Bytes 1..8 contribute 7 bits each with
// bit 7 as the continuation flag; byte 9 contributes all 8 bits so a full
// 64-bit integer fits in 9 bytes (sqlite3GetVarint, src/util.c:1633-1781).
func Varint(b []byte) (uint64, int, error) {
	var value uint64
	for i := 0; i < MaxVarintLength-1; i++ {
		if i >= len(b) {
			return 0, 0, errVarintTruncated
		}
		value = value<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return value, i + 1, nil
		}
	}
	if len(b) < MaxVarintLength {
		return 0, 0, errVarintTruncated
	}
	// The 9th byte has no continuation bit; all 8 bits belong to the value.
	value = value<<8 | uint64(b[MaxVarintLength-1])
	return value, MaxVarintLength, nil
}

// AppendVarint encodes value as an SQLite 1-to-9-byte varint and appends it to dst.
func AppendVarint(dst []byte, value uint64) []byte {
	// Values above 2^56 - 1 require all 9 bytes, with the low 8 bits in byte 9.
	if value > 0x00ffffffffffffff {
		var out [MaxVarintLength]byte
		out[8] = byte(value)
		value >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(value&0x7f) | 0x80
			value >>= 7
		}
		return append(dst, out[:]...)
	}
	var reversed [MaxVarintLength]byte
	n := 0
	for {
		reversed[n] = byte(value&0x7f) | 0x80
		n++
		value >>= 7
		if value == 0 {
			break
		}
	}
	// Clear the continuation bit on the least-significant (last-emitted) byte.
	reversed[0] &= 0x7f
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, reversed[i])
	}
	return dst
}

// VarintLength returns the encoded byte length of value as an SQLite varint.
func VarintLength(value uint64) int {
	return len(AppendVarint(nil, value))
}
