package storage

import (
	"bytes"

	bolt "go.etcd.io/bbolt"
)

// This file exposes application state: read-only accessors for HTTP handlers and
// the dashboard, plus the single write path (Apply/StateTx) reserved for the
// deterministic state machine. Values are opaque encoded bytes; the state
// machine (internal/limiter) owns their concrete models and JSON encoding, so
// storage never imports application model types.

// --- read-only accessors (return copies safe to keep after the tx) ---

// Policy returns the encoded policy for id, and whether it exists.
func (s *Store) Policy(id string) ([]byte, bool, error) {
	return s.getAppValue(bucketPolicies, []byte(id))
}

// Policies returns every stored policy value.
func (s *Store) Policies() ([][]byte, error) { return s.listValues(bucketPolicies) }

// Client returns the encoded client for id, and whether it exists.
func (s *Store) Client(id string) ([]byte, bool, error) {
	return s.getAppValue(bucketClients, []byte(id))
}

// Clients returns every stored client value.
func (s *Store) Clients() ([][]byte, error) { return s.listValues(bucketClients) }

// ClientIDByPrefix resolves an API key prefix to its client ID.
func (s *Store) ClientIDByPrefix(prefix string) (string, bool, error) {
	v, ok, err := s.getAppValue(bucketClientPrefix, []byte(prefix))
	return string(v), ok, err
}

// TokenBucket returns the encoded bucket state for a policy/subject pair.
func (s *Store) TokenBucket(policyID, subject string) ([]byte, bool, error) {
	key, err := compositeKey([]byte(policyID), []byte(subject))
	if err != nil {
		return nil, false, err
	}
	return s.getAppValue(bucketTokenBuckets, key)
}

// Idempotency returns the stored decision record for a client/request pair.
func (s *Store) Idempotency(clientID, requestID string) ([]byte, bool, error) {
	key, err := compositeKey([]byte(clientID), []byte(requestID))
	if err != nil {
		return nil, false, err
	}
	return s.getAppValue(bucketIdempotency, key)
}

// RecentAudits returns up to limit audit values, newest first.
func (s *Store) RecentAudits(limit int) ([][]byte, error) {
	out := make([][]byte, 0)
	if limit <= 0 {
		return out, nil
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketAudit).Cursor()
		for k, v := c.Last(); k != nil && len(out) < limit; k, v = c.Prev() {
			out = append(out, cloneBytes(v))
		}
		return nil
	})
	return out, err
}

func (s *Store) getAppValue(bucket, key []byte) ([]byte, bool, error) {
	var (
		val   []byte
		found bool
	)
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucket).Get(key)
		if raw != nil {
			val = cloneBytes(raw)
			found = true
		}
		return nil
	})
	return val, found, err
}

func (s *Store) listValues(bucket []byte) ([][]byte, error) {
	out := make([][]byte, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, v []byte) error {
			out = append(out, cloneBytes(v))
			return nil
		})
	})
	return out, err
}

// --- state machine write path ---

// StateTx is the write surface handed to the state machine while applying a
// committed command. Every write here — plus the new last_applied checkpoint —
// commits in one bbolt transaction, so a crash leaves either all of an entry's
// effects or none.
type StateTx struct {
	tx *bolt.Tx
}

// Apply runs fn inside one read-write transaction. Only the state machine
// should call this. If fn returns an error the whole transaction rolls back.
func (s *Store) Apply(fn func(*StateTx) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return fn(&StateTx{tx: tx})
	})
}

// GetPolicy reads a policy within the transaction.
func (t *StateTx) GetPolicy(id string) ([]byte, bool) {
	return getTx(t.tx, bucketPolicies, []byte(id))
}

// PutPolicy writes a policy within the transaction.
func (t *StateTx) PutPolicy(id string, val []byte) error {
	return t.tx.Bucket(bucketPolicies).Put([]byte(id), val)
}

// GetClient reads a client within the transaction.
func (t *StateTx) GetClient(id string) ([]byte, bool) {
	return getTx(t.tx, bucketClients, []byte(id))
}

// PutClient writes a client within the transaction.
func (t *StateTx) PutClient(id string, val []byte) error {
	return t.tx.Bucket(bucketClients).Put([]byte(id), val)
}

// GetClientIDByPrefix resolves an API key prefix within the transaction.
func (t *StateTx) GetClientIDByPrefix(prefix string) (string, bool) {
	v, ok := getTx(t.tx, bucketClientPrefix, []byte(prefix))
	return string(v), ok
}

// PutClientPrefix maps an API key prefix to a client ID within the transaction.
func (t *StateTx) PutClientPrefix(prefix, clientID string) error {
	return t.tx.Bucket(bucketClientPrefix).Put([]byte(prefix), []byte(clientID))
}

// GetTokenBucket reads a token bucket within the transaction.
func (t *StateTx) GetTokenBucket(policyID, subject string) ([]byte, bool, error) {
	key, err := compositeKey([]byte(policyID), []byte(subject))
	if err != nil {
		return nil, false, err
	}
	v, ok := getTx(t.tx, bucketTokenBuckets, key)
	return v, ok, nil
}

// PutTokenBucket writes a token bucket within the transaction.
func (t *StateTx) PutTokenBucket(policyID, subject string, val []byte) error {
	key, err := compositeKey([]byte(policyID), []byte(subject))
	if err != nil {
		return err
	}
	return t.tx.Bucket(bucketTokenBuckets).Put(key, val)
}

// GetIdempotency reads a decision record within the transaction.
func (t *StateTx) GetIdempotency(clientID, requestID string) ([]byte, bool, error) {
	key, err := compositeKey([]byte(clientID), []byte(requestID))
	if err != nil {
		return nil, false, err
	}
	v, ok := getTx(t.tx, bucketIdempotency, key)
	return v, ok, nil
}

// PutIdempotency writes a decision record within the transaction.
func (t *StateTx) PutIdempotency(clientID, requestID string, val []byte) error {
	key, err := compositeKey([]byte(clientID), []byte(requestID))
	if err != nil {
		return err
	}
	return t.tx.Bucket(bucketIdempotency).Put(key, val)
}

// PutAudit appends an audit value keyed by committed log index.
func (t *StateTx) PutAudit(logIndex uint64, val []byte) error {
	return t.tx.Bucket(bucketAudit).Put(u64(logIndex), val)
}

// SetLastApplied records the applied checkpoint in this same transaction.
func (t *StateTx) SetLastApplied(index uint64) error {
	return t.tx.Bucket(bucketMeta).Put(metaLastApplied, u64(index))
}

// ForEachTokenBucketByPolicy calls fn for every token bucket belonging to
// policyID, in key order. Do not mutate the bucket from within fn; collect
// results and write them after this returns (iteration must not be disturbed).
func (t *StateTx) ForEachTokenBucketByPolicy(policyID string, fn func(subject string, val []byte) error) error {
	prefix, err := compositeKey([]byte(policyID))
	if err != nil {
		return err
	}
	c := t.tx.Bucket(bucketTokenBuckets).Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		parts, err := splitCompositeKey(k)
		if err != nil {
			return err
		}
		if len(parts) != 2 || string(parts[0]) != policyID {
			continue
		}
		if err := fn(string(parts[1]), cloneBytes(v)); err != nil {
			return err
		}
	}
	return nil
}

// ForEachIdempotency calls fn for each idempotency record (raw key + value).
// Collect keys to delete and delete them after iteration, not during.
func (t *StateTx) ForEachIdempotency(fn func(key, val []byte) error) error {
	return t.tx.Bucket(bucketIdempotency).ForEach(func(k, v []byte) error {
		return fn(cloneBytes(k), cloneBytes(v))
	})
}

// DeleteIdempotency removes an idempotency record by its raw composite key.
func (t *StateTx) DeleteIdempotency(key []byte) error {
	return t.tx.Bucket(bucketIdempotency).Delete(key)
}

// ForEachAudit calls fn for each audit record in ascending log-index order.
// Collect indexes to delete and delete them after iteration, not during.
func (t *StateTx) ForEachAudit(fn func(index uint64, val []byte) error) error {
	return t.tx.Bucket(bucketAudit).ForEach(func(k, v []byte) error {
		return fn(parseU64(k), cloneBytes(v))
	})
}

// DeleteAudit removes an audit record by its log index.
func (t *StateTx) DeleteAudit(index uint64) error {
	return t.tx.Bucket(bucketAudit).Delete(u64(index))
}

// getTx returns a copy of a value within a transaction.
func getTx(tx *bolt.Tx, bucket, key []byte) ([]byte, bool) {
	raw := tx.Bucket(bucket).Get(key)
	if raw == nil {
		return nil, false
	}
	return cloneBytes(raw), true
}
