package dbfile

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf16"
)

type StorageClass int

const (
	Null StorageClass = iota
	Integer
	Real
	Text
	Blob
)

func (s StorageClass) String() string {
	switch s {
	case Null:
		return "null"
	case Integer:
		return "integer"
	case Real:
		return "real"
	case Text:
		return "text"
	case Blob:
		return "blob"
	}
	return fmt.Sprintf("class(%d)", int(s))
}

type Value struct {
	SerialType uint64
	Class      StorageClass
	Int        int64
	Float      float64
	Bytes      []byte
}

func (v Value) String() string {
	switch v.Class {
	case Null:
		return "NULL"
	case Integer:
		return fmt.Sprint(v.Int)
	case Real:
		return fmt.Sprint(v.Float)
	case Text:
		return fmt.Sprintf("%q", v.Bytes)
	}
	return fmt.Sprintf("x'%x'", v.Bytes)
}

func SerialTypeLength(t uint64) (int, error) {
	switch {
	case t <= 4:
		return []int{0, 1, 2, 3, 4}[t], nil
	case t == 5:
		return 6, nil
	case t == 6 || t == 7:
		return 8, nil
	case t == 8 || t == 9:
		return 0, nil
	case t == 10 || t == 11:
		return 0, fmt.Errorf("serial type %d is reserved", t)
	case t > 2*MaxPayloadSize+13:
		return 0, fmt.Errorf("serial type %d is larger than any payload", t)
	}
	return int((t - 12) / 2), nil
}

func signedBigEndian(b []byte) int64 {
	var value int64
	if len(b) > 0 && b[0]&0x80 != 0 {
		value = -1
	}
	for _, c := range b {
		value = value<<8 | int64(c)
	}
	return value
}

func DecodeValue(t uint64, body []byte) (Value, error) {
	length, err := SerialTypeLength(t)
	if err != nil {
		return Value{}, err
	}
	if length > len(body) {
		return Value{}, fmt.Errorf("serial type %d needs %d bytes, %d left", t, length, len(body))
	}
	v := Value{SerialType: t}
	switch {
	case t == 0:
		v.Class = Null
	case t <= 6:
		v.Class = Integer
		v.Int = signedBigEndian(body[:length])
	case t == 7:
		v.Class = Real
		v.Float = math.Float64frombits(binary.BigEndian.Uint64(body[:8]))
		if math.IsNaN(v.Float) {
			v.Class = Null
			v.Float = 0
		}
	case t == 8 || t == 9:
		v.Class = Integer
		v.Int = int64(t - 8)
	case t%2 == 0:
		v.Class = Blob
		v.Bytes = body[:length]
	default:
		v.Class = Text
		v.Bytes = body[:length]
	}
	return v, nil
}

func DecodeRecord(payload []byte) ([]Value, error) {
	headerSize, n, err := Varint(payload)
	if err != nil {
		return nil, fmt.Errorf("record header size: %w", err)
	}
	if headerSize < uint64(n) || headerSize > uint64(len(payload)) {
		return nil, fmt.Errorf("record header size %d is outside %d..%d", headerSize, n, len(payload))
	}
	header := payload[n:headerSize]
	body := payload[headerSize:]
	var values []Value
	for len(header) > 0 {
		t, used, err := Varint(header)
		if err != nil {
			return nil, fmt.Errorf("record column %d serial type: %w", len(values), err)
		}
		header = header[used:]
		v, err := DecodeValue(t, body)
		if err != nil {
			return nil, fmt.Errorf("record column %d: %w", len(values), err)
		}
		length, _ := SerialTypeLength(t)
		body = body[length:]
		values = append(values, v)
	}
	return values, nil
}

func DecodeText(raw []byte, encoding uint32) (string, error) {
	switch encoding {
	case 0, TextEncodingUTF8:
		return string(raw), nil
	case TextEncodingUTF16L, TextEncodingUTF16B:
		if len(raw)%2 != 0 {
			return "", fmt.Errorf("UTF-16 text has an odd length %d", len(raw))
		}
		units := make([]uint16, len(raw)/2)
		for i := range units {
			if encoding == TextEncodingUTF16L {
				units[i] = binary.LittleEndian.Uint16(raw[2*i:])
			} else {
				units[i] = binary.BigEndian.Uint16(raw[2*i:])
			}
		}
		return string(utf16.Decode(units)), nil
	}
	return "", fmt.Errorf("unknown text encoding %d", encoding)
}
