// Package limiter holds the policy, client, and bucket models, the versioned
// replicated command types, and the deterministic integer token-bucket state
// machine.
//
// Determinism is the contract: applying the same command sequence in the same
// order to a fresh store always yields byte-identical state on every node. The
// apply path therefore uses only integer arithmetic (no floating point), the
// timestamp carried inside each command (never a local clock), and no
// randomness, goroutines, network calls, or environment access.
package limiter
