package storage

import (
	"bytes"
	"encoding/hex"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// appBuckets are the application-state buckets that must converge across nodes
// once they reach the same applied index. Excludes meta (term/vote differ per
// node) and raft_log.
var appBuckets = [][]byte{
	bucketPolicies, bucketClients, bucketClientPrefix,
	bucketTokenBuckets, bucketIdempotency, bucketAudit,
}

// DebugDumpApp returns a deterministic snapshot of the application-state buckets
// only, for asserting that nodes at the same applied index hold identical
// application state (term/vote/log naturally differ and are excluded).
func (s *Store) DebugDumpApp() ([]byte, error) {
	return s.dumpBuckets(appBuckets)
}

// DebugDump returns a deterministic, human-readable snapshot of every bucket in
// key order. It is a test and diagnostic aid for comparing that two nodes that
// applied the same commands reached byte-identical application state. It must
// not be used on a hot path.
func (s *Store) DebugDump() ([]byte, error) {
	return s.dumpBuckets(allBuckets)
}

func (s *Store) dumpBuckets(names [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, name := range names {
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
