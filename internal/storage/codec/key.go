package codec

import (
	"encoding/binary"
	"fmt"
)

// Table tags (first byte of the key), DESIGN §5.
const (
	KeyNode  byte = 'n' // n + nodeID(8)
	KeyEdge  byte = 'e' // e + edgeID(8)
	KeyOut   byte = 'o' // o + src(8) + type(4) + dst(8) + edge(8)
	KeyIn    byte = 'i' // i + dst(8) + type(4) + src(8) + edge(8)
	KeyLabel byte = 'l' // l + label(4) + node(8)
	KeyProp  byte = 'p' // p + label(4) + propKey(4) + valEnc + node(8)
)

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

func appendU64(dst []byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(dst, b[:]...)
}

// NodeKey: node record.
func NodeKey(id uint64) []byte { return appendU64([]byte{KeyNode}, id) }

// EdgeKey: edge record.
func EdgeKey(id uint64) []byte { return appendU64([]byte{KeyEdge}, id) }

// NodePrefix: scan of all nodes (fallback).
func NodePrefix() []byte { return []byte{KeyNode} }

// OutKey: outgoing edge from src.
func OutKey(src uint64, typeID uint32, dst, edge uint64) []byte {
	k := make([]byte, 0, 1+8+4+8+8)
	k = append(k, KeyOut)
	k = appendU64(k, src)
	k = appendU32(k, typeID)
	k = appendU64(k, dst)
	k = appendU64(k, edge)
	return k
}

// InKey: incoming edge into dst.
func InKey(dst uint64, typeID uint32, src, edge uint64) []byte {
	k := make([]byte, 0, 1+8+4+8+8)
	k = append(k, KeyIn)
	k = appendU64(k, dst)
	k = appendU32(k, typeID)
	k = appendU64(k, src)
	k = appendU64(k, edge)
	return k
}

// OutPrefix: scan of the outgoing edges from src of a given type.
func OutPrefix(src uint64, typeID uint32) []byte {
	return appendU32(appendU64([]byte{KeyOut}, src), typeID)
}

// OutPrefixAll: scan of all outgoing edges from src (any type).
func OutPrefixAll(src uint64) []byte { return appendU64([]byte{KeyOut}, src) }

// InPrefix: scan of the incoming edges into dst of a given type.
func InPrefix(dst uint64, typeID uint32) []byte {
	return appendU32(appendU64([]byte{KeyIn}, dst), typeID)
}

// InPrefixAll: scan of all incoming edges into dst (any type).
func InPrefixAll(dst uint64) []byte { return appendU64([]byte{KeyIn}, dst) }

// LabelKey: entry of the label index.
func LabelKey(label uint32, node uint64) []byte {
	return appendU64(appendU32([]byte{KeyLabel}, label), node)
}

// LabelPrefix: scan of the nodes with a given label.
func LabelPrefix(label uint32) []byte { return appendU32([]byte{KeyLabel}, label) }

// PropKey: entry of the secondary property index.
func PropKey(label, propKey uint32, value any, node uint64) ([]byte, error) {
	k := make([]byte, 0, 1+4+4+16+8)
	k = append(k, KeyProp)
	k = appendU32(k, label)
	k = appendU32(k, propKey)
	k, err := AppendIndexValue(k, value)
	if err != nil {
		return nil, err
	}
	return appendU64(k, node), nil
}

// PropPrefix: scan of the index entries with equality on a value.
func PropPrefix(label, propKey uint32, value any) ([]byte, error) {
	k := make([]byte, 0, 1+4+4+16)
	k = append(k, KeyProp)
	k = appendU32(k, label)
	k = appendU32(k, propKey)
	return AppendIndexValue(k, value)
}

// ParseOutKey extracts the components from an `o` key.
func ParseOutKey(key []byte) (src uint64, typeID uint32, dst, edge uint64, err error) {
	if len(key) != 1+8+4+8+8 || key[0] != KeyOut {
		return 0, 0, 0, 0, fmt.Errorf("codec: malformed `o` key (len=%d)", len(key))
	}
	src = binary.BigEndian.Uint64(key[1:9])
	typeID = binary.BigEndian.Uint32(key[9:13])
	dst = binary.BigEndian.Uint64(key[13:21])
	edge = binary.BigEndian.Uint64(key[21:29])
	return src, typeID, dst, edge, nil
}

// ParseInKey extracts the components from an `i` key.
func ParseInKey(key []byte) (dst uint64, typeID uint32, src, edge uint64, err error) {
	if len(key) != 1+8+4+8+8 || key[0] != KeyIn {
		return 0, 0, 0, 0, fmt.Errorf("codec: malformed `i` key (len=%d)", len(key))
	}
	dst = binary.BigEndian.Uint64(key[1:9])
	typeID = binary.BigEndian.Uint32(key[9:13])
	src = binary.BigEndian.Uint64(key[13:21])
	edge = binary.BigEndian.Uint64(key[21:29])
	return dst, typeID, src, edge, nil
}

// ParseLabelKey extracts label and nodeID from an `l` key.
func ParseLabelKey(key []byte) (label uint32, node uint64, err error) {
	if len(key) != 1+4+8 || key[0] != KeyLabel {
		return 0, 0, fmt.Errorf("codec: malformed `l` key (len=%d)", len(key))
	}
	label = binary.BigEndian.Uint32(key[1:5])
	node = binary.BigEndian.Uint64(key[5:13])
	return label, node, nil
}

// ParsePropKeyNode extracts the nodeID (last 8 bytes) from a `p` key.
func ParsePropKeyNode(key []byte) (uint64, error) {
	if len(key) < 1+4+4+1+8 || key[0] != KeyProp {
		return 0, fmt.Errorf("codec: malformed `p` key (len=%d)", len(key))
	}
	return binary.BigEndian.Uint64(key[len(key)-8:]), nil
}

// NodeIDFromKey extracts the ID from an `n` key.
func NodeIDFromKey(key []byte) (uint64, error) {
	if len(key) != 1+8 || key[0] != KeyNode {
		return 0, fmt.Errorf("codec: malformed `n` key (len=%d)", len(key))
	}
	return binary.BigEndian.Uint64(key[1:9]), nil
}
