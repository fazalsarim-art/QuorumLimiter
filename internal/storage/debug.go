package storage

import (
	"bytes"
	"encoding/hex"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// DebugDump returns a deterministic, human-readable snapshot of every bucket in
// key order. It is a test and diagnostic aid for comparing that two nodes that
// applied the same commands reached byte-identical application state. It must
// not be used on a hot path.
func (s *Store) DebugDump() ([]byte, error) {
	var buf bytes.Buffer
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			fmt.Fprintf(&buf, "== %s ==\n", name)
			b := tx.Bucket(name)
			if b == nil {
				continue
			}
			// bbolt iterates keys in byte-sorted order, so output is stable.
			if err := b.ForEach(func(k, v []byte) error {
				fmt.Fprintf(&buf, "%s\t%s\n", hex.EncodeToString(k), v)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
