package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Type tags for indexed values, in ordered bands (DESIGN §5):
// NULL < BOOL < INT < FLOAT < STRING. The tag order determines the cross-type
// order in the `p` index keys.
const (
	tagNull  byte = 0x00
	tagBool  byte = 0x01
	tagInt   byte = 0x02
	tagFloat byte = 0x03
	tagStr   byte = 0x04
)

// Sentinels for the "ordered bytes" string encoding.
const (
	strEsc  byte = 0x00 // byte to escape
	strFF   byte = 0xFF // 0x00 -> 0x00 0xFF
	strTerm byte = 0x00 // terminator 0x00 0x00
)

// errShort signals a truncated buffer during decoding.
var errShort = errors.New("codec: buffer too short")

// AppendIndexValue appends the order-preserving encoding of v to dst.
// Indexable types: nil, bool, int64, float64, string. The encoding guarantees
// that bytes.Compare respects the logical order for values of the same type, and
// the per-type-band order across different types.
func AppendIndexValue(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, tagNull), nil
	case bool:
		b := byte(0)
		if x {
			b = 1
		}
		return append(dst, tagBool, b), nil
	case int64:
		var buf [8]byte
		// Sign-bit flip: makes two's complement sortable as unsigned.
		binary.BigEndian.PutUint64(buf[:], uint64(x)^(1<<63))
		dst = append(dst, tagInt)
		return append(dst, buf[:]...), nil
	case float64:
		bits := math.Float64bits(x)
		if bits&(1<<63) != 0 {
			// Negative: invert all bits so negatives order among themselves.
			bits = ^bits
		} else {
			// Non-negative: set the high bit so it sorts after negatives.
			bits |= 1 << 63
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], bits)
		dst = append(dst, tagFloat)
		return append(dst, buf[:]...), nil
	case string:
		dst = append(dst, tagStr)
		return appendOrderedString(dst, x), nil
	default:
		return nil, fmt.Errorf("codec: non-indexable type %T", v)
	}
}

// DecodeIndexValue decodes one value from the start of src, returning the value
// and the number of bytes consumed (useful because in `p` keys the value is
// followed by the nodeID).
func DecodeIndexValue(src []byte) (any, int, error) {
	if len(src) == 0 {
		return nil, 0, errShort
	}
	switch src[0] {
	case tagNull:
		return nil, 1, nil
	case tagBool:
		if len(src) < 2 {
			return nil, 0, errShort
		}
		return src[1] != 0, 2, nil
	case tagInt:
		if len(src) < 9 {
			return nil, 0, errShort
		}
		u := binary.BigEndian.Uint64(src[1:9]) ^ (1 << 63)
		return int64(u), 9, nil
	case tagFloat:
		if len(src) < 9 {
			return nil, 0, errShort
		}
		bits := binary.BigEndian.Uint64(src[1:9])
		if bits&(1<<63) != 0 {
			bits &^= 1 << 63
		} else {
			bits = ^bits
		}
		return math.Float64frombits(bits), 9, nil
	case tagStr:
		s, n, err := decodeOrderedString(src[1:])
		if err != nil {
			return nil, 0, err
		}
		return s, 1 + n, nil
	default:
		return nil, 0, fmt.Errorf("codec: unknown value tag 0x%02x", src[0])
	}
}

// appendOrderedString appends the string with the "ordered bytes" encoding:
// each 0x00 becomes 0x00 0xFF, and the 0x00 0x00 terminator is appended.
// No length prefix: it would break the lexicographic order.
func appendOrderedString(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == strEsc {
			dst = append(dst, strEsc, strFF)
		} else {
			dst = append(dst, s[i])
		}
	}
	return append(dst, strTerm, strTerm)
}

// decodeOrderedString reads an "ordered bytes" string up to the terminator,
// returning the value and the bytes consumed (terminator included).
func decodeOrderedString(src []byte) (string, int, error) {
	var b []byte
	for i := 0; i < len(src); {
		c := src[i]
		if c != strEsc {
			b = append(b, c)
			i++
			continue
		}
		if i+1 >= len(src) {
			return "", 0, errShort
		}
		switch src[i+1] {
		case strTerm: // 0x00 0x00 -> end
			return string(b), i + 2, nil
		case strFF: // 0x00 0xFF -> 0x00
			b = append(b, strEsc)
			i += 2
		default:
			return "", 0, fmt.Errorf("codec: invalid escape sequence 0x00 0x%02x", src[i+1])
		}
	}
	return "", 0, errShort
}
