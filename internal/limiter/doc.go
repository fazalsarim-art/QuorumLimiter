// Package limiter holds the policy and bucket models, the versioned replicated
// command types, and the deterministic integer token-bucket state machine.
//
// Implementation begins in Phase 3. Nothing in the apply path may read a clock,
// environment variable, or network, or use floating point.
package limiter
