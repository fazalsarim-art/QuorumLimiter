package integration

import (
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/testcluster"
)

// TestIdempotentDecisionThroughCluster verifies that replaying a decision's
// idempotency key through the committed log returns the original result and does
// not charge twice.
func TestIdempotentDecisionThroughCluster(t *testing.T) {
	c := testcluster.New(t, 3)
	c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)

	first := mustPropose(t, c, decideCmd("cli", "pol", "s", 2, "req-idem-000000001", 2000))
	if first.Outcome != limiter.OutcomeAllowed || first.Duplicate {
		t.Fatalf("first: outcome %s dup %v", first.Outcome, first.Duplicate)
	}
	remaining := first.Decision.RemainingMilli

	// Replay the same idempotency key (later timestamp): original result, no charge.
	replay := mustPropose(t, c, decideCmd("cli", "pol", "s", 2, "req-idem-000000001", 9999))
	if !replay.Duplicate {
		t.Error("replay should be marked duplicate")
	}
	if replay.Decision.RemainingMilli != remaining {
		t.Errorf("replay remaining = %d, want %d (double charge?)", replay.Decision.RemainingMilli, remaining)
	}

	// A fresh request reflects only the first charge.
	fresh := mustPropose(t, c, decideCmd("cli", "pol", "s", 1, "req-fresh-000000002", 2000))
	if fresh.Decision.RemainingMilli != remaining-1000 {
		t.Errorf("balance after replay = %d, want %d", fresh.Decision.RemainingMilli, remaining-1000)
	}

	c.WaitAppConverged(3*time.Second, "node1", "node2", "node3")
}

// TestIdempotencyConflictThroughCluster verifies a reused key with different
// content is rejected as a conflict.
func TestIdempotencyConflictThroughCluster(t *testing.T) {
	c := testcluster.New(t, 3)
	c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)

	mustPropose(t, c, decideCmd("cli", "pol", "s", 1, "req-conflict-00001", 2000))
	conflict := mustPropose(t, c, decideCmd("cli", "pol", "s", 3, "req-conflict-00001", 2000))
	if conflict.Outcome != limiter.OutcomeRejected || conflict.RejectCode != limiter.RejectIdempotencyConflict {
		t.Fatalf("conflict: outcome %s code %s", conflict.Outcome, conflict.RejectCode)
	}
}
