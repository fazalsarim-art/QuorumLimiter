// Package raft implements the from-scratch Raft-lite consensus layer: roles and
// state, the serialized event loop, elections, log replication, commit
// advancement, ordered apply, and RPC message types.
//
// Implementation begins in Phase 4. This package must not import HTTP handlers
// or dashboard code.
package raft
