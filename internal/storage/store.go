package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// openTimeout bounds how long Open waits for the file lock before failing,
	// surfacing "another process holds this database" quickly.
	openTimeout = 1 * time.Second
	dbFileMode  = 0o600
	dbDirMode   = 0o700
)

// Store owns one node's bbolt database. It is the single component that knows
// bucket names and value encodings. Only two writers exist: the Raft
// persistence methods (term, vote, log, checkpoints) and the state machine via
// Apply. HTTP handlers never write application buckets directly.
type Store struct {
	db *bolt.DB
}

// Open opens (creating if needed) the bbolt file at path, ensuring the parent
// directory exists, then runs schema migrations. The caller must Close it.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dbDirMode); err != nil {
			return nil, fmt.Errorf("storage: create data dir %q: %w", dir, err)
		}
	}
	db, err := bolt.Open(path, dbFileMode, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", path, err)
	}
	if err := db.Update(migrate); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: migrate %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the database file path.
func (s *Store) Path() string { return s.db.Path() }

// StoredSchemaVersion returns the schema version recorded in the database.
func (s *Store) StoredSchemaVersion() (uint64, error) {
	return s.getMetaU64(metaSchemaVersion)
}

// --- small shared meta helpers ---

func (s *Store) getMetaU64(key []byte) (uint64, error) {
	var v uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		v = parseU64(tx.Bucket(bucketMeta).Get(key))
		return nil
	})
	return v, err
}

func (s *Store) setMetaU64(key []byte, v uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(key, u64(v))
	})
}

func (s *Store) getMetaString(key []byte) (string, error) {
	var v string
	err := s.db.View(func(tx *bolt.Tx) error {
		v = string(tx.Bucket(bucketMeta).Get(key))
		return nil
	})
	return v, err
}

func (s *Store) setMetaString(key []byte, v string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(key, []byte(v))
	})
}
