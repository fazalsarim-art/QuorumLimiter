package storage

import (
	"errors"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// SchemaVersion is the schema version this binary understands.
const SchemaVersion uint64 = 1

// Bucket names. Kept private so storage is the single owner of the schema.
var (
	bucketMeta         = []byte("meta")
	bucketRaftLog      = []byte("raft_log")
	bucketPolicies     = []byte("policies")
	bucketClients      = []byte("clients")
	bucketClientPrefix = []byte("client_prefix")
	bucketTokenBuckets = []byte("token_buckets")
	bucketIdempotency  = []byte("idempotency")
	bucketAudit        = []byte("audit")
)

// allBuckets is the full set created by the version 1 migration.
var allBuckets = [][]byte{
	bucketMeta, bucketRaftLog, bucketPolicies, bucketClients,
	bucketClientPrefix, bucketTokenBuckets, bucketIdempotency, bucketAudit,
}

// Keys within the meta bucket.
var (
	metaSchemaVersion = []byte("schema_version")
	metaCurrentTerm   = []byte("current_term")
	metaVotedFor      = []byte("voted_for")
	metaCommitIndex   = []byte("commit_index")
	metaLastApplied   = []byte("last_applied")
	metaClusterID     = []byte("cluster_id")
)

// ErrSchemaTooNew is returned when the stored schema version is newer than this
// binary supports. Startup must refuse rather than risk misinterpreting data.
var ErrSchemaTooNew = errors.New("storage: database schema is newer than this binary supports")

// migrate brings the schema up to SchemaVersion within one writable
// transaction. It is idempotent: reopening an up-to-date database makes no
// changes.
func migrate(tx *bolt.Tx) error {
	current := readSchemaVersion(tx)
	if current > SchemaVersion {
		return fmt.Errorf("%w (stored %d, supported %d)", ErrSchemaTooNew, current, SchemaVersion)
	}
	for v := current + 1; v <= SchemaVersion; v++ {
		if err := applyMigration(tx, v); err != nil {
			return fmt.Errorf("storage: migration to v%d: %w", v, err)
		}
		if err := tx.Bucket(bucketMeta).Put(metaSchemaVersion, u64(v)); err != nil {
			return fmt.Errorf("storage: record schema v%d: %w", v, err)
		}
	}
	return nil
}

// readSchemaVersion returns the stored schema version, or 0 if the database is
// brand new (no meta bucket yet).
func readSchemaVersion(tx *bolt.Tx) uint64 {
	b := tx.Bucket(bucketMeta)
	if b == nil {
		return 0
	}
	return parseU64(b.Get(metaSchemaVersion))
}

func applyMigration(tx *bolt.Tx, version uint64) error {
	switch version {
	case 1:
		return migrateV1(tx)
	default:
		return fmt.Errorf("no migration defined for v%d", version)
	}
}

// migrateV1 creates every bucket and installs the index-zero log sentinel. It
// is safe to run more than once.
func migrateV1(tx *bolt.Tx) error {
	for _, name := range allBuckets {
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return fmt.Errorf("create bucket %q: %w", name, err)
		}
	}
	// The sentinel entry at index 0, term 0 simplifies previous-entry checks
	// during replication. It is not an application command and never appears in
	// audits.
	logb := tx.Bucket(bucketRaftLog)
	if logb.Get(u64(0)) == nil {
		enc, err := encodeLogEntry(LogEntry{Index: 0, Term: 0, Kind: KindSentinel})
		if err != nil {
			return err
		}
		if err := logb.Put(u64(0), enc); err != nil {
			return fmt.Errorf("install log sentinel: %w", err)
		}
	}
	return nil
}
