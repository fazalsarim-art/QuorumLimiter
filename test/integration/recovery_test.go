package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/testcluster"
)

func TestRestartFollowerCatchesUp(t *testing.T) {
	c := testcluster.New(t, 3)
	leader := c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)
	for i := 0; i < 10; i++ {
		mustPropose(t, c, decideCmd("cli", "pol", "s", 1, fmt.Sprintf("req-pre-%08d", i), int64(2000+i)))
	}

	// Restart a follower; it must recover from disk and catch up via replication.
	var follower string
	for _, id := range []string{"node1", "node2", "node3"} {
		if id != leader.ID {
			follower = id
			break
		}
	}
	c.Restart(follower)

	// More traffic after the restart.
	for i := 0; i < 5; i++ {
		mustPropose(t, c, decideCmd("cli", "pol", "s", 1, fmt.Sprintf("req-post-%08d", i), int64(3000+i)))
	}
	c.WaitAppConverged(5*time.Second, "node1", "node2", "node3")
}

func TestFullClusterRestartRecoversState(t *testing.T) {
	c := testcluster.New(t, 3)
	c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)
	for i := 0; i < 8; i++ {
		mustPropose(t, c, decideCmd("cli", "pol", "s", 1, fmt.Sprintf("req-%08d", i), int64(2000+i)))
	}

	// Restart every node; each recovers term, vote, log, and applied state from
	// disk, then the cluster re-elects a leader and stays converged.
	for _, id := range []string{"node1", "node2", "node3"} {
		c.Restart(id)
	}

	c.WaitLeader(5 * time.Second)
	c.WaitAppConverged(5*time.Second, "node1", "node2", "node3")
}
