package storage

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Log entry kinds.
const (
	// KindSentinel is the index-0 base entry. It is not an application command.
	KindSentinel = "sentinel"
	// KindNoop is the no-op a new leader appends in its term.
	KindNoop = "noop"
	// KindCommand carries a replicated application command envelope.
	KindCommand = "command"
)

// LogEntry is one entry in the replicated Raft log, exactly as persisted. The
// Command payload is an opaque versioned command envelope owned by the state
// machine; storage does not interpret it.
type LogEntry struct {
	Index   uint64 `json:"index"`
	Term    uint64 `json:"term"`
	Kind    string `json:"kind"`
	Command []byte `json:"command,omitempty"`
}

func encodeLogEntry(e LogEntry) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("storage: encode log entry %d: %w", e.Index, err)
	}
	return b, nil
}

func decodeLogEntry(b []byte) (LogEntry, error) {
	var e LogEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return LogEntry{}, fmt.Errorf("storage: decode log entry: %w", err)
	}
	return e, nil
}

// --- persistent term and vote ---

// CurrentTerm returns the persisted current term (0 if never set).
func (s *Store) CurrentTerm() (uint64, error) { return s.getMetaU64(metaCurrentTerm) }

// SetCurrentTerm persists the current term.
func (s *Store) SetCurrentTerm(term uint64) error { return s.setMetaU64(metaCurrentTerm, term) }

// VotedFor returns the node voted for in the current term ("" if none).
func (s *Store) VotedFor() (string, error) { return s.getMetaString(metaVotedFor) }

// SetVotedFor persists the vote for the current term.
func (s *Store) SetVotedFor(nodeID string) error { return s.setMetaString(metaVotedFor, nodeID) }

// SetTermAndVote persists both term and vote in one transaction, as a candidate
// must before requesting votes. Persisting both atomically prevents voting
// twice in one term after a crash between the two writes.
func (s *Store) SetTermAndVote(term uint64, votedFor string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketMeta)
		if err := mb.Put(metaCurrentTerm, u64(term)); err != nil {
			return fmt.Errorf("storage: set term: %w", err)
		}
		if err := mb.Put(metaVotedFor, []byte(votedFor)); err != nil {
			return fmt.Errorf("storage: set vote: %w", err)
		}
		return nil
	})
}

// --- cluster identity ---

// ClusterID returns the persisted cluster ID ("" if never set).
func (s *Store) ClusterID() (string, error) { return s.getMetaString(metaClusterID) }

// SetClusterID persists the cluster ID.
func (s *Store) SetClusterID(id string) error { return s.setMetaString(metaClusterID, id) }

// --- commit and applied checkpoints ---

// CommitIndex returns the persisted commit checkpoint.
func (s *Store) CommitIndex() (uint64, error) { return s.getMetaU64(metaCommitIndex) }

// SetCommitIndex persists the commit checkpoint.
func (s *Store) SetCommitIndex(index uint64) error { return s.setMetaU64(metaCommitIndex, index) }

// LastApplied returns the persisted applied checkpoint.
func (s *Store) LastApplied() (uint64, error) { return s.getMetaU64(metaLastApplied) }

// SetLastApplied persists the applied checkpoint. Note: when applying a command,
// the state machine must set last_applied inside the same Apply transaction as
// the application writes; see StateTx.SetLastApplied.
func (s *Store) SetLastApplied(index uint64) error { return s.setMetaU64(metaLastApplied, index) }

// --- replicated log ---

// AppendEntries writes entries in ascending index order. Existing indexes are
// overwritten, which is how a leader's authoritative entries replace a
// follower's conflicting ones (paired with TruncateSuffix in later phases).
func (s *Store) AppendEntries(entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRaftLog)
		for _, e := range entries {
			enc, err := encodeLogEntry(e)
			if err != nil {
				return err
			}
			if err := b.Put(u64(e.Index), enc); err != nil {
				return fmt.Errorf("storage: append entry %d: %w", e.Index, err)
			}
		}
		return nil
	})
}

// Entry returns the log entry at index and whether it exists.
func (s *Store) Entry(index uint64) (LogEntry, bool, error) {
	var (
		e     LogEntry
		found bool
	)
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketRaftLog).Get(u64(index))
		if raw == nil {
			return nil
		}
		var derr error
		e, derr = decodeLogEntry(raw)
		if derr != nil {
			return derr
		}
		found = true
		return nil
	})
	return e, found, err
}

// Entries returns the entries with index in [lo, hi] inclusive, ascending.
func (s *Store) Entries(lo, hi uint64) ([]LogEntry, error) {
	var out []LogEntry
	if hi < lo {
		return out, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketRaftLog).Cursor()
		for k, v := c.Seek(u64(lo)); k != nil; k, v = c.Next() {
			if parseU64(k) > hi {
				break
			}
			e, derr := decodeLogEntry(v)
			if derr != nil {
				return derr
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

// FirstIndex returns the lowest stored log index (normally the sentinel, 0).
func (s *Store) FirstIndex() (uint64, error) {
	var idx uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(bucketRaftLog).Cursor().First()
		idx = parseU64(k)
		return nil
	})
	return idx, err
}

// LastIndex returns the highest stored log index.
func (s *Store) LastIndex() (uint64, error) {
	var idx uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(bucketRaftLog).Cursor().Last()
		idx = parseU64(k)
		return nil
	})
	return idx, err
}

// TruncateSuffix deletes every entry with index >= from. The index-0 sentinel
// is never removed.
func (s *Store) TruncateSuffix(from uint64) error {
	if from == 0 {
		from = 1
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return truncateSuffixTx(tx, from)
	})
}

// OverwriteEntries atomically repairs a follower's log: if truncateFrom >= 1 it
// deletes the conflicting suffix (index >= truncateFrom, never the sentinel),
// then appends entries — all in one transaction, so a crash leaves the log
// either unchanged or fully repaired.
func (s *Store) OverwriteEntries(truncateFrom uint64, entries []LogEntry) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if truncateFrom >= 1 {
			if err := truncateSuffixTx(tx, truncateFrom); err != nil {
				return err
			}
		}
		b := tx.Bucket(bucketRaftLog)
		for _, e := range entries {
			enc, err := encodeLogEntry(e)
			if err != nil {
				return err
			}
			if err := b.Put(u64(e.Index), enc); err != nil {
				return fmt.Errorf("storage: overwrite entry %d: %w", e.Index, err)
			}
		}
		return nil
	})
}

func truncateSuffixTx(tx *bolt.Tx, from uint64) error {
	b := tx.Bucket(bucketRaftLog)
	c := b.Cursor()
	var keys [][]byte
	for k, _ := c.Seek(u64(from)); k != nil; k, _ = c.Next() {
		keys = append(keys, cloneBytes(k))
	}
	for _, k := range keys {
		if err := b.Delete(k); err != nil {
			return fmt.Errorf("storage: truncate entry: %w", err)
		}
	}
	return nil
}
