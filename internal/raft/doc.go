// Package raft implements the from-scratch Raft-lite consensus layer: roles and
// state, the serialized event loop, elections, log replication, commit
// advancement, ordered apply, and RPC message types.
//
// A node runs a single event-loop goroutine that exclusively owns all mutable
// consensus state; callers interact only through channels, so the state stays
// race-free without a shared mutex. The package depends on narrow injected
// interfaces (Store, Transport, ApplyFunc, Clock) and must not import HTTP
// handlers or dashboard code.
package raft
