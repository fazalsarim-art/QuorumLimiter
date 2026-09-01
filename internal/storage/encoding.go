package storage

import (
	"encoding/binary"
	"fmt"
)

// maxKeyPartLen bounds each part of a composite key. Values above this cannot be
// length-prefixed in two bytes. Real validation elsewhere is far smaller.
const maxKeyPartLen = 65535

// u64 encodes v as 8-byte big-endian so keys sort numerically.
func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// parseU64 decodes an 8-byte big-endian value, returning 0 if b is not 8 bytes.
func parseU64(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// compositeKey builds an unambiguous key from parts, each prefixed by its
// 2-byte big-endian length. Length prefixing prevents a separator collision
// when a part (such as a subject) could itself contain any byte.
//
//	[len(part0)][part0][len(part1)][part1]...
func compositeKey(parts ...[]byte) ([]byte, error) {
	total := 0
	for _, p := range parts {
		if len(p) > maxKeyPartLen {
			return nil, fmt.Errorf("storage: key part length %d exceeds %d", len(p), maxKeyPartLen)
		}
		total += 2 + len(p)
	}
	out := make([]byte, 0, total)
	var lenbuf [2]byte
	for _, p := range parts {
		binary.BigEndian.PutUint16(lenbuf[:], uint16(len(p)))
		out = append(out, lenbuf[0], lenbuf[1])
		out = append(out, p...)
	}
	return out, nil
}

// splitCompositeKey parses a key built by compositeKey back into its parts.
func splitCompositeKey(key []byte) ([][]byte, error) {
	var parts [][]byte
	for len(key) > 0 {
		if len(key) < 2 {
			return nil, fmt.Errorf("storage: malformed composite key")
		}
		n := int(binary.BigEndian.Uint16(key[:2]))
		key = key[2:]
		if len(key) < n {
			return nil, fmt.Errorf("storage: malformed composite key")
		}
		parts = append(parts, key[:n])
		key = key[n:]
	}
	return parts, nil
}

// cloneBytes returns a copy so callers may retain data after a bbolt
// transaction ends; bbolt-owned slices are only valid for the life of the tx.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
