package raft

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

// memTransport routes RPCs directly to peer nodes in-process, exercising real
// concurrent election behavior without HTTP.
type memTransport struct {
	mu    sync.Mutex
	nodes map[string]*Node
}

func newMemTransport() *memTransport { return &memTransport{nodes: map[string]*Node{}} }

func (t *memTransport) register(n *Node) {
	t.mu.Lock()
	t.nodes[n.id] = n
	t.mu.Unlock()
}

func (t *memTransport) lookup(id string) *Node {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nodes[id]
}

var errUnreachable = errors.New("peer unreachable")

func (t *memTransport) SendRequestVote(ctx context.Context, target string, req RequestVoteRequest) (RequestVoteResponse, error) {
	n := t.lookup(target)
	if n == nil {
		return RequestVoteResponse{}, errUnreachable
	}
	return n.HandleRequestVote(ctx, req)
}

func (t *memTransport) SendAppendEntries(ctx context.Context, target string, req AppendEntriesRequest) (AppendEntriesResponse, error) {
	n := t.lookup(target)
	if n == nil {
		return AppendEntriesResponse{}, errUnreachable
	}
	return n.HandleAppendEntries(ctx, req)
}

func clusterConfig(id string, ids []string) Config {
	return Config{
		NodeID:             id,
		ClusterID:          "test-cluster",
		Peers:              ids,
		ElectionTimeoutMin: 60 * time.Millisecond,
		ElectionTimeoutMax: 120 * time.Millisecond,
		HeartbeatInterval:  20 * time.Millisecond,
	}
}

// buildCluster creates and starts a three-node in-process cluster, returning the
// nodes, the shared transport, and each node's store (for restart tests).
func buildCluster(t *testing.T) ([]*Node, *memTransport, map[string]*storage.Store) {
	t.Helper()
	ids := []string{"node1", "node2", "node3"}
	tr := newMemTransport()
	var nodes []*Node
	stores := make(map[string]*storage.Store)

	for _, id := range ids {
		st := openStore(t)
		stores[id] = st
		n, err := newNode(clusterConfig(id, ids), Deps{Store: st, Transport: tr})
		if err != nil {
			t.Fatalf("newNode %s: %v", id, err)
		}
		tr.register(n)
		nodes = append(nodes, n)
	}
	for _, n := range nodes {
		n.start()
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			n.Stop()
		}
	})
	return nodes, tr, stores
}

// waitForLeader polls until exactly one of the given nodes is leader, failing on
// timeout or if two leaders are ever seen in the same term.
func waitForLeader(t *testing.T, nodes []*Node, timeout time.Duration) *Node {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var leader *Node
		count := 0
		termLeaders := map[uint64]int{}
		for _, n := range nodes {
			s, err := n.Status()
			if err != nil {
				continue
			}
			if s.Role == RoleLeader {
				count++
				leader = n
				termLeaders[s.Term]++
			}
		}
		for term, c := range termLeaders {
			if c > 1 {
				t.Fatalf("safety violation: %d leaders in term %d", c, term)
			}
		}
		if count == 1 {
			return leader
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no single leader elected within timeout")
	return nil
}

func waitForRole(t *testing.T, n *Node, want Role, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s, err := n.Status(); err == nil && s.Role == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node %s did not reach role %s within timeout", n.id, want)
}

func without(nodes []*Node, exclude *Node) []*Node {
	var out []*Node
	for _, n := range nodes {
		if n != exclude {
			out = append(out, n)
		}
	}
	return out
}

func TestElectionSingleLeader(t *testing.T) {
	nodes, _, _ := buildCluster(t)
	leader := waitForLeader(t, nodes, 3*time.Second)

	s, _ := leader.Status()
	if s.Term == 0 {
		t.Errorf("leader term = 0, want >= 1")
	}
}

func TestElectionLeaderFailover(t *testing.T) {
	nodes, _, _ := buildCluster(t)
	leader := waitForLeader(t, nodes, 3*time.Second)
	oldTerm := func() uint64 { s, _ := leader.Status(); return s.Term }()

	// Stop the leader; the remaining two must elect a new one in a higher term.
	leader.Stop()
	remaining := without(nodes, leader)
	newLeader := waitForLeader(t, remaining, 3*time.Second)

	if newLeader == leader {
		t.Fatal("new leader is the stopped node")
	}
	s, _ := newLeader.Status()
	if s.Term <= oldTerm {
		t.Errorf("new leader term %d, want > %d", s.Term, oldTerm)
	}
}

func TestElectionOldLeaderRestartsAsFollower(t *testing.T) {
	nodes, tr, stores := buildCluster(t)
	leader := waitForLeader(t, nodes, 3*time.Second)
	leaderID := leader.id

	leader.Stop()
	remaining := without(nodes, leader)
	waitForLeader(t, remaining, 3*time.Second)

	// Restart the old leader from its store; it must rejoin as a follower.
	restarted, err := newNode(clusterConfig(leaderID, []string{"node1", "node2", "node3"}), Deps{
		Store: stores[leaderID], Transport: tr,
	})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	tr.register(restarted)
	restarted.start()
	t.Cleanup(restarted.Stop)

	waitForRole(t, restarted, RoleFollower, 3*time.Second)
}
