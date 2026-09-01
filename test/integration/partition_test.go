package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
	"github.com/fazalsarim-art/QuorumLimiter/internal/testcluster"
)

func TestPartitionedFollowerCatchesUpAfterHeal(t *testing.T) {
	c := testcluster.New(t, 3)
	leader := c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)

	// Isolate a follower; the leader and the other follower keep a quorum.
	var follower string
	for _, id := range []string{"node1", "node2", "node3"} {
		if id != leader.ID {
			follower = id
			break
		}
	}
	c.Isolate(follower)

	for i := 0; i < 8; i++ {
		mustPropose(t, c, decideCmd("cli", "pol", "s", 1, fmt.Sprintf("req-part-%08d", i), int64(2000+i)))
	}

	// Heal; the lagging follower must converge with the others.
	c.Heal()
	c.WaitAppConverged(6*time.Second, "node1", "node2", "node3")
}

// TestOldLeaderConflictRepaired forces a conflicting suffix: an isolated old
// leader appends uncommitted entries, a new leader commits different entries at
// the same indexes, and on heal the old leader's conflicting suffix is repaired.
func TestOldLeaderConflictRepaired(t *testing.T) {
	c := testcluster.New(t, 3)
	oldLeader := c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)

	others := []string{}
	for _, id := range []string{"node1", "node2", "node3"} {
		if id != oldLeader.ID {
			others = append(others, id)
		}
	}

	// Isolate the old leader and give it uncommitted entries (they time out).
	c.Isolate(oldLeader.ID)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		body, _ := decideCmd("cli", "pol", "old", 1, fmt.Sprintf("req-old-%08d", i), int64(2000+i)).Encode()
		_, _ = oldLeader.Raft.Propose(ctx, body) // expected to time out
		cancel()
	}

	// The majority partition elects a new leader.
	var newLeaderID string
	testcluster.Eventually(t, 3*time.Second, func() bool {
		for _, id := range others {
			if s, err := c.Member(id).Raft.Status(); err == nil && s.Role == raft.RoleLeader {
				newLeaderID = id
				return true
			}
		}
		return false
	}, "no new leader in majority partition", c.Diagnostics)

	// Commit different entries (via the new leader) at the same indexes.
	newLeader := c.Member(newLeaderID)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		body, _ := decideCmd("cli", "pol", "new", 1, fmt.Sprintf("req-new-%08d", i), int64(5000+i)).Encode()
		if _, err := newLeader.Raft.Propose(ctx, body); err != nil {
			cancel()
			t.Fatalf("new leader propose: %v", err)
		}
		cancel()
	}

	// Heal: the old leader steps down and its conflicting suffix is replaced.
	c.Heal()
	c.WaitAppConverged(6*time.Second, "node1", "node2", "node3")
}
