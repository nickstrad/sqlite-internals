package dbfile

import "errors"

var errVarintTruncated = errors.New("varint runs past the end of its buffer")

const MaxVarintLength = 9

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
	value = value<<8 | uint64(b[MaxVarintLength-1])
	return value, MaxVarintLength, nil
}

func AppendVarint(dst []byte, value uint64) []byte {
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
	reversed[0] &= 0x7f
	for i := n - 1; i >= 0; i-- {
		dst = append(dst, reversed[i])
	}
	return dst
}

func VarintLength(value uint64) int {
	return len(AppendVarint(nil, value))
}
