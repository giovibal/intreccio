package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Tag di tipo per i valori indicizzati, in bande ordinate (DESIGN §5):
// NULL < BOOL < INT < FLOAT < STRING. L'ordine dei tag determina l'ordine
// cross-tipo nelle chiavi dell'indice `p`.
const (
	tagNull  byte = 0x00
	tagBool  byte = 0x01
	tagInt   byte = 0x02
	tagFloat byte = 0x03
	tagStr   byte = 0x04
)

// Sentinelle per i valori "ordered bytes" delle stringhe.
const (
	strEsc  byte = 0x00 // byte da escapare
	strFF   byte = 0xFF // 0x00 -> 0x00 0xFF
	strTerm byte = 0x00 // terminatore 0x00 0x00
)

// errShort indica un buffer troncato durante il decoding.
var errShort = errors.New("codec: buffer troppo corto")

// AppendIndexValue accoda a dst l'encoding order-preserving di v.
// Tipi indicizzabili: nil, bool, int64, float64, string. L'encoding garantisce
// che bytes.Compare rispetti l'ordine logico per valori dello stesso tipo, e
// l'ordine per banda di tipo tra tipi diversi.
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
		// Flip del bit di segno: rende il complemento a due ordinabile come unsigned.
		binary.BigEndian.PutUint64(buf[:], uint64(x)^(1<<63))
		dst = append(dst, tagInt)
		return append(dst, buf[:]...), nil
	case float64:
		bits := math.Float64bits(x)
		if bits&(1<<63) != 0 {
			// Negativo: inverti tutti i bit così i negativi ordinano tra loro.
			bits = ^bits
		} else {
			// Non negativo: setta il bit alto così ordina dopo i negativi.
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
		return nil, fmt.Errorf("codec: tipo non indicizzabile %T", v)
	}
}

// DecodeIndexValue decodifica un valore dall'inizio di src, restituendo il valore
// e il numero di byte consumati (utile perché nelle chiavi `p` il valore è
// seguito dal nodeID).
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
		return nil, 0, fmt.Errorf("codec: tag valore sconosciuto 0x%02x", src[0])
	}
}

// appendOrderedString accoda la stringa con encoding "ordered bytes":
// ogni 0x00 diventa 0x00 0xFF, e in coda si aggiunge il terminatore 0x00 0x00.
// Niente length-prefix: romperebbe l'ordine lessicografico.
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

// decodeOrderedString legge una stringa "ordered bytes" fino al terminatore,
// restituendo il valore e i byte consumati (terminatore incluso).
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
		case strTerm: // 0x00 0x00 -> fine
			return string(b), i + 2, nil
		case strFF: // 0x00 0xFF -> 0x00
			b = append(b, strEsc)
			i += 2
		default:
			return "", 0, fmt.Errorf("codec: sequenza di escape non valida 0x00 0x%02x", src[i+1])
		}
	}
	return "", 0, errShort
}
