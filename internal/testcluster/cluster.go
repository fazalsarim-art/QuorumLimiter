package testcluster

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

// Timing tuned for fast, reliable in-process tests.
const (
	electionMin = 60 * time.Millisecond
	electionMax = 120 * time.Millisecond
	heartbeat   = 20 * time.Millisecond

	clusterID = "testcluster"
)

// Member is one node in the cluster.
type Member struct {
	ID    string
	Raft  *raft.Node
	Store *storage.Store
	path  string
	cfg   raft.Config
}

// Cluster is an in-process set of QuorumLimiter nodes sharing a fault transport.
type Cluster struct {
	t         *testing.T
	dir       string
	ids       []string
	transport *FaultTransport
	sm        *limiter.StateMachine
	members   map[string]*Member
}

// New builds and starts a cluster of the given size.
func New(t *testing.T, size int) *Cluster {
	t.Helper()
	ids := make([]string, size)
	for i := range ids {
		ids[i] = fmt.Sprintf("node%d", i+1)
	}
	c := &Cluster{
		t:         t,
		dir:       t.TempDir(),
		ids:       ids,
		transport: NewFaultTransport(),
		sm:        limiter.New(),
		members:   make(map[string]*Member, size),
	}
	for _, id := range ids {
		c.members[id] = c.newMember(id)
	}
	t.Cleanup(c.stopAll)
	return c
}

func (c *Cluster) config(id string) raft.Config {
	return raft.Config{
		NodeID:             id,
		ClusterID:          clusterID,
		Peers:              c.ids,
		ElectionTimeoutMin: electionMin,
		ElectionTimeoutMax: electionMax,
		HeartbeatInterval:  heartbeat,
	}
}

// newMember opens storage, wires the state machine as the apply function, starts
// the raft node, and registers it with the transport.
func (c *Cluster) newMember(id string) *Member {
	c.t.Helper()
	path := filepath.Join(c.dir, id, "db")
	st, err := storage.Open(path)
	if err != nil {
		c.t.Fatalf("open store %s: %v", id, err)
	}
	cfg := c.config(id)
	rn := c.startRaft(id, cfg, st)
	return &Member{ID: id, Raft: rn, Store: st, path: path, cfg: cfg}
}

// startRaft constructs a raft node bound to store st and registers it.
func (c *Cluster) startRaft(id string, cfg raft.Config, st *storage.Store) *raft.Node {
	apply := func(index, term uint64, entry raft.LogEntry) (any, error) {
		return c.sm.ApplyEntry(st, index, term, entry)
	}
	rn, err := raft.New(cfg, raft.Deps{
		Store:     st,
		Transport: c.transport,
		Apply:     apply,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		c.t.Fatalf("start raft %s: %v", id, err)
	}
	c.transport.register(id, rn)
	return rn
}

func (c *Cluster) stopAll() {
	for _, m := range c.members {
		m.Raft.Stop()
		_ = m.Store.Close()
	}
}

// Member returns the member with the given id.
func (c *Cluster) Member(id string) *Member { return c.members[id] }

// Transport exposes the fault transport for partition control.
func (c *Cluster) Transport() *FaultTransport { return c.transport }

// StateMachine returns the shared (stateless) state machine.
func (c *Cluster) StateMachine() *limiter.StateMachine { return c.sm }

// status returns a member's status, or a zero status if it is stopped.
func (c *Cluster) status(id string) raft.Status {
	s, err := c.members[id].Raft.Status()
	if err != nil {
		return raft.Status{NodeID: id}
	}
	return s
}

// Leader returns the current unique leader, or nil if there is not exactly one.
func (c *Cluster) Leader() *Member {
	var leader *Member
	count := 0
	for _, id := range c.ids {
		if c.status(id).Role == raft.RoleLeader {
			leader = c.members[id]
			count++
		}
	}
	if count == 1 {
		return leader
	}
	return nil
}

// WaitLeader blocks until exactly one leader exists, failing on timeout or a
// same-term double-leader safety violation.
func (c *Cluster) WaitLeader(timeout time.Duration) *Member {
	c.t.Helper()
	var leader *Member
	Eventually(c.t, timeout, func() bool {
		termLeaders := map[uint64]int{}
		var found *Member
		n := 0
		for _, id := range c.ids {
			s := c.status(id)
			if s.Role == raft.RoleLeader {
				termLeaders[s.Term]++
				found = c.members[id]
				n++
			}
		}
		for term, cnt := range termLeaders {
			if cnt > 1 {
				c.t.Fatalf("safety violation: %d leaders in term %d", cnt, term)
			}
		}
		if n == 1 {
			leader = found
			return true
		}
		return false
	}, "no single leader elected", c.Diagnostics)
	return leader
}

// Propose sends a command to the current leader and returns the applied result.
func (c *Cluster) Propose(ctx context.Context, cmd limiter.Command) (limiter.Result, error) {
	leader := c.WaitLeader(2 * time.Second)
	body, err := cmd.Encode()
	if err != nil {
		return limiter.Result{}, err
	}
	res, err := leader.Raft.Propose(ctx, body)
	if err != nil {
		return limiter.Result{}, err
	}
	r, ok := res.(limiter.Result)
	if !ok {
		return limiter.Result{}, fmt.Errorf("unexpected result type %T", res)
	}
	return r, nil
}

// Stop makes a node unavailable (stops its raft loop; storage stays open).
func (c *Cluster) Stop(id string) {
	c.members[id].Raft.Stop()
}

// Restart simulates a process restart: stop, close storage, reopen the same
// file, and start a fresh raft node that recovers from disk.
func (c *Cluster) Restart(id string) {
	m := c.members[id]
	m.Raft.Stop()
	if err := m.Store.Close(); err != nil {
		c.t.Fatalf("close store %s: %v", id, err)
	}
	st, err := storage.Open(m.path)
	if err != nil {
		c.t.Fatalf("reopen store %s: %v", id, err)
	}
	m.Store = st
	m.Raft = c.startRaft(id, m.cfg, st)
}

// Partition cuts communication in both directions between a and b.
func (c *Cluster) Partition(a, b string) {
	c.transport.Block(a, b)
	c.transport.Block(b, a)
}

// Isolate cuts a node off from all others (both directions).
func (c *Cluster) Isolate(id string) {
	for _, other := range c.ids {
		if other != id {
			c.Partition(id, other)
		}
	}
}

// Heal restores all links.
func (c *Cluster) Heal() {
	for _, a := range c.ids {
		for _, b := range c.ids {
			if a != b {
				c.transport.Unblock(a, b)
			}
		}
	}
}

// WaitAppConverged waits until all listed members share the same applied index
// and identical application state.
func (c *Cluster) WaitAppConverged(timeout time.Duration, ids ...string) {
	c.t.Helper()
	if len(ids) == 0 {
		ids = c.ids
	}
	Eventually(c.t, timeout, func() bool {
		var wantApplied uint64
		var wantDump string
		for i, id := range ids {
			s := c.status(id)
			dump, err := c.members[id].Store.DebugDumpApp()
			if err != nil {
				return false
			}
			if i == 0 {
				wantApplied = s.LastApplied
				wantDump = string(dump)
				continue
			}
			if s.LastApplied != wantApplied || string(dump) != wantDump {
				return false
			}
		}
		return true
	}, "nodes did not converge on applied index and application state", c.Diagnostics)
}

// Diagnostics returns a human-readable per-node state summary for test failures.
func (c *Cluster) Diagnostics() string {
	var b []byte
	for _, id := range c.ids {
		s := c.status(id)
		b = append(b, []byte(fmt.Sprintf(
			"  %s: role=%s term=%d leader=%q lastLog=%d commit=%d applied=%d\n",
			id, s.Role, s.Term, s.LeaderID, s.LastLogIndex, s.CommitIndex, s.LastApplied))...)
	}
	return string(b)
}
