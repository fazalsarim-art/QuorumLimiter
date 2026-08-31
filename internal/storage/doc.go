// Package storage owns the per-node bbolt database: bucket definitions and
// migrations, encoding helpers, Raft persistence (term, vote, log, checkpoints),
// and application-state reads and the state-machine write transaction.
//
// Implementation begins in Phase 2.
package storage
