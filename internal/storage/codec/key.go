package codec

import (
	"encoding/binary"
	"fmt"
)

// Tag delle tabelle (primo byte della chiave), DESIGN §5.
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

// NodeKey: record nodo.
func NodeKey(id uint64) []byte { return appendU64([]byte{KeyNode}, id) }

// EdgeKey: record arco.
func EdgeKey(id uint64) []byte { return appendU64([]byte{KeyEdge}, id) }

// NodePrefix: scan di tutti i nodi (fallback).
func NodePrefix() []byte { return []byte{KeyNode} }

// OutKey: arco uscente da src.
func OutKey(src uint64, typeID uint32, dst, edge uint64) []byte {
	k := make([]byte, 0, 1+8+4+8+8)
	k = append(k, KeyOut)
	k = appendU64(k, src)
	k = appendU32(k, typeID)
	k = appendU64(k, dst)
	k = appendU64(k, edge)
	return k
}

// InKey: arco entrante in dst.
func InKey(dst uint64, typeID uint32, src, edge uint64) []byte {
	k := make([]byte, 0, 1+8+4+8+8)
	k = append(k, KeyIn)
	k = appendU64(k, dst)
	k = appendU32(k, typeID)
	k = appendU64(k, src)
	k = appendU64(k, edge)
	return k
}

// OutPrefix: scan degli archi uscenti da src di un dato tipo.
func OutPrefix(src uint64, typeID uint32) []byte {
	return appendU32(appendU64([]byte{KeyOut}, src), typeID)
}

// OutPrefixAll: scan di tutti gli archi uscenti da src (qualsiasi tipo).
func OutPrefixAll(src uint64) []byte { return appendU64([]byte{KeyOut}, src) }

// InPrefix: scan degli archi entranti in dst di un dato tipo.
func InPrefix(dst uint64, typeID uint32) []byte {
	return appendU32(appendU64([]byte{KeyIn}, dst), typeID)
}

// InPrefixAll: scan di tutti gli archi entranti in dst (qualsiasi tipo).
func InPrefixAll(dst uint64) []byte { return appendU64([]byte{KeyIn}, dst) }

// LabelKey: entry dell'indice per label.
func LabelKey(label uint32, node uint64) []byte {
	return appendU64(appendU32([]byte{KeyLabel}, label), node)
}

// LabelPrefix: scan dei nodi con una data label.
func LabelPrefix(label uint32) []byte { return appendU32([]byte{KeyLabel}, label) }

// PropKey: entry dell'indice secondario su proprietà.
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

// PropPrefix: scan delle entry dell'indice con equality su un valore.
func PropPrefix(label, propKey uint32, value any) ([]byte, error) {
	k := make([]byte, 0, 1+4+4+16)
	k = append(k, KeyProp)
	k = appendU32(k, label)
	k = appendU32(k, propKey)
	return AppendIndexValue(k, value)
}

// ParseOutKey estrae i componenti da una chiave `o`.
func ParseOutKey(key []byte) (src uint64, typeID uint32, dst, edge uint64, err error) {
	if len(key) != 1+8+4+8+8 || key[0] != KeyOut {
		return 0, 0, 0, 0, fmt.Errorf("codec: chiave `o` malformata (len=%d)", len(key))
	}
	src = binary.BigEndian.Uint64(key[1:9])
	typeID = binary.BigEndian.Uint32(key[9:13])
	dst = binary.BigEndian.Uint64(key[13:21])
	edge = binary.BigEndian.Uint64(key[21:29])
	return src, typeID, dst, edge, nil
}

// ParseInKey estrae i componenti da una chiave `i`.
func ParseInKey(key []byte) (dst uint64, typeID uint32, src, edge uint64, err error) {
	if len(key) != 1+8+4+8+8 || key[0] != KeyIn {
		return 0, 0, 0, 0, fmt.Errorf("codec: chiave `i` malformata (len=%d)", len(key))
	}
	dst = binary.BigEndian.Uint64(key[1:9])
	typeID = binary.BigEndian.Uint32(key[9:13])
	src = binary.BigEndian.Uint64(key[13:21])
	edge = binary.BigEndian.Uint64(key[21:29])
	return dst, typeID, src, edge, nil
}

// ParseLabelKey estrae label e nodeID da una chiave `l`.
func ParseLabelKey(key []byte) (label uint32, node uint64, err error) {
	if len(key) != 1+4+8 || key[0] != KeyLabel {
		return 0, 0, fmt.Errorf("codec: chiave `l` malformata (len=%d)", len(key))
	}
	label = binary.BigEndian.Uint32(key[1:5])
	node = binary.BigEndian.Uint64(key[5:13])
	return label, node, nil
}

// ParsePropKeyNode estrae il nodeID (ultimi 8 byte) da una chiave `p`.
func ParsePropKeyNode(key []byte) (uint64, error) {
	if len(key) < 1+4+4+1+8 || key[0] != KeyProp {
		return 0, fmt.Errorf("codec: chiave `p` malformata (len=%d)", len(key))
	}
	return binary.BigEndian.Uint64(key[len(key)-8:]), nil
}

// NodeIDFromKey estrae l'ID da una chiave `n`.
func NodeIDFromKey(key []byte) (uint64, error) {
	if len(key) != 1+8 || key[0] != KeyNode {
		return 0, fmt.Errorf("codec: chiave `n` malformata (len=%d)", len(key))
	}
	return binary.BigEndian.Uint64(key[1:9]), nil
}
