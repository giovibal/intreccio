package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Tag di tipo per i valori dentro i record `n`/`e`. A differenza dei valori
// indicizzati (`valEnc`), qui conta solo il round-trip, non l'ordine: si usa un
// formato self-describing length-prefixed che supporta anche list e map.
const (
	recNull  byte = 0
	recBool  byte = 1
	recInt   byte = 2
	recFloat byte = 3
	recStr   byte = 4
	recList  byte = 5
	recMap   byte = 6
)

// NodeRecord è il valore serializzato della chiave `n`.
type NodeRecord struct {
	Labels []uint32       // ID di label, ordinati ascendenti
	Props  map[uint32]any // propKeyID -> valore
}

// EdgeRecord è il valore serializzato della chiave `e`.
type EdgeRecord struct {
	Type  uint32
	Src   uint64
	Dst   uint64
	Props map[uint32]any
}

// EncodeNode serializza un record nodo.
func EncodeNode(rec NodeRecord) ([]byte, error) {
	labels := append([]uint32(nil), rec.Labels...)
	sort.Slice(labels, func(i, j int) bool { return labels[i] < labels[j] })

	dst := make([]byte, 0, 16+len(labels)*2)
	dst = binary.AppendUvarint(dst, uint64(len(labels)))
	for _, l := range labels {
		dst = binary.AppendUvarint(dst, uint64(l))
	}
	return appendProps(dst, rec.Props)
}

// DecodeNode deserializza un record nodo. Props è sempre non-nil.
func DecodeNode(b []byte) (NodeRecord, error) {
	var rec NodeRecord
	r := &reader{b: b}
	n, err := r.uvarint()
	if err != nil {
		return rec, fmt.Errorf("codec: record nodo, numero label: %w", err)
	}
	rec.Labels = make([]uint32, n)
	for i := range rec.Labels {
		l, err := r.uvarint()
		if err != nil {
			return rec, fmt.Errorf("codec: record nodo, label %d: %w", i, err)
		}
		rec.Labels[i] = uint32(l)
	}
	rec.Props, err = r.props()
	if err != nil {
		return rec, err
	}
	return rec, nil
}

// EncodeEdge serializza un record arco.
func EncodeEdge(rec EdgeRecord) ([]byte, error) {
	dst := make([]byte, 0, 32)
	dst = binary.AppendUvarint(dst, uint64(rec.Type))
	dst = binary.AppendUvarint(dst, rec.Src)
	dst = binary.AppendUvarint(dst, rec.Dst)
	return appendProps(dst, rec.Props)
}

// DecodeEdge deserializza un record arco. Props è sempre non-nil.
func DecodeEdge(b []byte) (EdgeRecord, error) {
	var rec EdgeRecord
	r := &reader{b: b}
	typ, err := r.uvarint()
	if err != nil {
		return rec, fmt.Errorf("codec: record arco, type: %w", err)
	}
	rec.Type = uint32(typ)
	if rec.Src, err = r.uvarint(); err != nil {
		return rec, fmt.Errorf("codec: record arco, src: %w", err)
	}
	if rec.Dst, err = r.uvarint(); err != nil {
		return rec, fmt.Errorf("codec: record arco, dst: %w", err)
	}
	rec.Props, err = r.props()
	if err != nil {
		return rec, err
	}
	return rec, nil
}

// appendProps serializza la mappa di proprietà con chiavi ordinate (encoding
// deterministico).
func appendProps(dst []byte, props map[uint32]any) ([]byte, error) {
	keys := make([]uint32, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	dst = binary.AppendUvarint(dst, uint64(len(keys)))
	for _, k := range keys {
		dst = binary.AppendUvarint(dst, uint64(k))
		var err error
		dst, err = appendRecValue(dst, props[k])
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

func appendRecValue(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, recNull), nil
	case bool:
		b := byte(0)
		if x {
			b = 1
		}
		return append(dst, recBool, b), nil
	case int:
		return appendInt(dst, int64(x)), nil
	case int64:
		return appendInt(dst, x), nil
	case float64:
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], math.Float64bits(x))
		return append(append(dst, recFloat), buf[:]...), nil
	case string:
		dst = append(dst, recStr)
		dst = binary.AppendUvarint(dst, uint64(len(x)))
		return append(dst, x...), nil
	case []any:
		dst = append(dst, recList)
		dst = binary.AppendUvarint(dst, uint64(len(x)))
		for _, e := range x {
			var err error
			if dst, err = appendRecValue(dst, e); err != nil {
				return nil, err
			}
		}
		return dst, nil
	case map[string]any:
		dst = append(dst, recMap)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = binary.AppendUvarint(dst, uint64(len(keys)))
		for _, k := range keys {
			dst = binary.AppendUvarint(dst, uint64(len(k)))
			dst = append(dst, k...)
			var err error
			if dst, err = appendRecValue(dst, x[k]); err != nil {
				return nil, err
			}
		}
		return dst, nil
	default:
		return nil, fmt.Errorf("codec: tipo proprietà non supportato %T", v)
	}
}

func appendInt(dst []byte, v int64) []byte {
	return binary.AppendVarint(append(dst, recInt), v)
}

// reader consuma un buffer in sequenza.
type reader struct {
	b []byte
	i int
}

func (r *reader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.i:])
	if n <= 0 {
		return 0, errShort
	}
	r.i += n
	return v, nil
}

func (r *reader) varint() (int64, error) {
	v, n := binary.Varint(r.b[r.i:])
	if n <= 0 {
		return 0, errShort
	}
	r.i += n
	return v, nil
}

func (r *reader) byte() (byte, error) {
	if r.i >= len(r.b) {
		return 0, errShort
	}
	c := r.b[r.i]
	r.i++
	return c, nil
}

func (r *reader) bytes(n int) ([]byte, error) {
	if r.i+n > len(r.b) {
		return nil, errShort
	}
	out := r.b[r.i : r.i+n]
	r.i += n
	return out, nil
}

func (r *reader) props() (map[uint32]any, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, fmt.Errorf("codec: numero proprietà: %w", err)
	}
	props := make(map[uint32]any, n)
	for i := uint64(0); i < n; i++ {
		k, err := r.uvarint()
		if err != nil {
			return nil, fmt.Errorf("codec: chiave proprietà: %w", err)
		}
		v, err := r.value()
		if err != nil {
			return nil, fmt.Errorf("codec: valore proprietà %d: %w", k, err)
		}
		props[uint32(k)] = v
	}
	return props, nil
}

func (r *reader) value() (any, error) {
	tag, err := r.byte()
	if err != nil {
		return nil, err
	}
	switch tag {
	case recNull:
		return nil, nil
	case recBool:
		b, err := r.byte()
		if err != nil {
			return nil, err
		}
		return b != 0, nil
	case recInt:
		return r.varint()
	case recFloat:
		raw, err := r.bytes(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(raw)), nil
	case recStr:
		n, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		raw, err := r.bytes(int(n))
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	case recList:
		n, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		list := make([]any, n)
		for i := range list {
			if list[i], err = r.value(); err != nil {
				return nil, err
			}
		}
		return list, nil
	case recMap:
		n, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, n)
		for i := uint64(0); i < n; i++ {
			klen, err := r.uvarint()
			if err != nil {
				return nil, err
			}
			kb, err := r.bytes(int(klen))
			if err != nil {
				return nil, err
			}
			v, err := r.value()
			if err != nil {
				return nil, err
			}
			m[string(kb)] = v
		}
		return m, nil
	default:
		return nil, fmt.Errorf("codec: tag record sconosciuto 0x%02x", tag)
	}
}
