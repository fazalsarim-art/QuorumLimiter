package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
	"github.com/fazalsarim-art/QuorumLimiter/internal/testcluster"
)

// --- command builders ---

var cmdSeq atomic.Int64

func newCmd(typ limiter.CommandType, ts int64, payload any) limiter.Command {
	c, err := limiter.NewCommand(fmt.Sprintf("cmd-%d", cmdSeq.Add(1)), typ, ts, payload)
	if err != nil {
		panic(err)
	}
	return c
}

func createPolicyCmd(id string, capacity, refill, interval, maxCost int64) limiter.Command {
	return newCmd(limiter.CmdCreatePolicy, 1000, limiter.CreatePolicyPayload{
		ID: id, Name: "Policy " + id, CapacityTokens: capacity,
		RefillTokens: refill, RefillIntervalMS: interval, MaxCostTokens: maxCost, Active: true,
	})
}

func createClientCmd(id string, allowed []string) limiter.Command {
	return newCmd(limiter.CmdCreateClient, 1000, limiter.CreateClientPayload{
		ID: id, Name: "Client " + id, KeyPrefix: "PFX" + id, KeyDigest: []byte("digest-" + id),
		AllowedPolicyIDs: allowed,
	})
}

func decideCmd(client, policy, subject string, cost int64, reqID string, ts int64) limiter.Command {
	return newCmd(limiter.CmdDecide, ts, limiter.DecidePayload{
		ClientID: client, PolicyID: policy, Subject: subject, Cost: cost, RequestID: reqID,
	})
}

func mustPropose(t *testing.T, c *testcluster.Cluster, cmd limiter.Command) limiter.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := c.Propose(ctx, cmd)
	if err != nil {
		t.Fatalf("propose %s: %v", cmd.Type, err)
	}
	return res
}

func seedPolicyAndClient(t *testing.T, c *testcluster.Cluster) {
	t.Helper()
	mustPropose(t, c, createPolicyCmd("pol", 20, 1, 3_600_000, 5))
	mustPropose(t, c, createClientCmd("cli", []string{"*"}))
}

// --- tests ---

func TestReplicationCommitsAndConverges(t *testing.T) {
	c := testcluster.New(t, 3)
	c.WaitLeader(3 * time.Second)

	res := mustPropose(t, c, createPolicyCmd("pol", 5, 1, 12000, 5))
	if res.Outcome != limiter.OutcomeApplied {
		t.Fatalf("create policy outcome %s", res.Outcome)
	}
	// The committed policy must be present and identical on all three nodes.
	c.WaitAppConverged(3*time.Second, "node1", "node2", "node3")
}

func TestFollowerUnavailableStillCommits(t *testing.T) {
	c := testcluster.New(t, 3)
	leader := c.WaitLeader(3 * time.Second)

	// Stop one follower; the remaining two form a quorum.
	var follower string
	for _, id := range []string{"node1", "node2", "node3"} {
		if id != leader.ID {
			follower = id
			break
		}
	}
	c.Stop(follower)

	res := mustPropose(t, c, createPolicyCmd("pol", 5, 1, 12000, 5))
	if res.Outcome != limiter.OutcomeApplied {
		t.Fatalf("commit with one follower down: %s", res.Outcome)
	}

	// The leader and the surviving follower converge.
	live := []string{}
	for _, id := range []string{"node1", "node2", "node3"} {
		if id != follower {
			live = append(live, id)
		}
	}
	c.WaitAppConverged(3*time.Second, live...)
}

func TestIsolatedLeaderCannotCommit(t *testing.T) {
	c := testcluster.New(t, 3)
	leader := c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)

	// Isolate the leader from both followers.
	c.Isolate(leader.ID)

	// A proposal to the isolated leader cannot reach a quorum and times out
	// (the entry is appended locally but never commits).
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	body, _ := decideCmd("cli", "pol", "s", 1, "req-isolated-000001", 2000).Encode()
	_, err := leader.Raft.Propose(ctx, body)
	if !errors.Is(err, raft.ErrProposalTimeout) && !errors.Is(err, raft.ErrNotLeader) {
		t.Fatalf("isolated leader proposal err = %v, want timeout/not-leader", err)
	}

	// Meanwhile the other two elect a new leader.
	var others []string
	for _, id := range []string{"node1", "node2", "node3"} {
		if id != leader.ID {
			others = append(others, id)
		}
	}
	testcluster.Eventually(t, 3*time.Second, func() bool {
		for _, id := range others {
			if c.Member(id).Raft != nil {
				if s, err := c.Member(id).Raft.Status(); err == nil && s.Role == raft.RoleLeader {
					return true
				}
			}
		}
		return false
	}, "no new leader among the majority partition", c.Diagnostics)
}

func TestConvergenceUnderManyDecisions(t *testing.T) {
	c := testcluster.New(t, 3)
	c.WaitLeader(3 * time.Second)
	seedPolicyAndClient(t, c)

	subjects := []string{"a", "b", "customer_4821", "d"}
	for i := 0; i < 60; i++ {
		subj := subjects[i%len(subjects)]
		cost := int64(1 + i%3)
		mustPropose(t, c, decideCmd("cli", "pol", subj, cost, fmt.Sprintf("req-conv-%08d", i), int64(2000+i)))
	}
	c.WaitAppConverged(3*time.Second, "node1", "node2", "node3")
}

func TestHotBucketConcurrencyNeverOverAllows(t *testing.T) {
	c := testcluster.New(t, 3)
	c.WaitLeader(3 * time.Second)
	// capacity 20, effectively no refill during the test.
	mustPropose(t, c, createPolicyCmd("pol", 20, 1, 3_600_000, 5))
	mustPropose(t, c, createClientCmd("cli", []string{"*"}))

	const attempts = 50
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			res, err := c.Propose(ctx, decideCmd("cli", "pol", "hot", 1, fmt.Sprintf("req-hot-%08d", i), 2000))
			if err != nil {
				t.Errorf("propose: %v", err)
				return
			}
			if res.Outcome == limiter.OutcomeAllowed {
				allowed.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := allowed.Load(); got != 20 {
		t.Errorf("allowed = %d, want exactly 20 (capacity)", got)
	}
	c.WaitAppConverged(3*time.Second, "node1", "node2", "node3")
}
