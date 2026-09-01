// Package raft implements the from-scratch Raft-lite consensus layer: roles and
// state, the serialized event loop, elections, log replication, commit
// advancement, ordered apply, and RPC message types.
//
// A node runs a single event-loop goroutine that exclusively owns all mutable
// consensus state; callers interact only through channels, so the state stays
// race-free without a shared mutex. The package depends on narrow injected
// interfaces (Store, Transport, ApplyFunc, Clock) and must not import HTTP
// handlers or dashboard code.
//
// It is built up across phases: Phase 4 covers types, persistent transitions,
// recovery, and status; Phase 5 adds leader election; Phase 6 adds log
// replication, quorum commit, and ordered apply.
package raft
