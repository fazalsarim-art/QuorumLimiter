package raft

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

// --- test doubles ---

// fakeClock is an injectable clock for deterministic timing tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	ch  chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0), ch: make(chan time.Time, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(time.Duration) <-chan time.Time { return c.ch }

// --- helpers ---

func openStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "n", "db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testConfig() Config {
	return Config{
		NodeID:             "node1",
		ClusterID:          "test-cluster",
		Peers:              []string{"node1", "node2", "node3"},
		ElectionTimeoutMin: 800 * time.Millisecond,
		ElectionTimeoutMax: 1400 * time.Millisecond,
		HeartbeatInterval:  200 * time.Millisecond,
	}
}

// noLoopNode builds a node without starting the event loop, for single-threaded
// transition tests.
func noLoopNode(t *testing.T, st *storage.Store) *Node {
	t.Helper()
	n, err := newNode(testConfig(), Deps{Store: st, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("newNode: %v", err)
	}
	t.Cleanup(n.cancel)
	return n
}

// runningNode builds a node with its event loop started.
func runningNode(t *testing.T, st *storage.Store) *Node {
	t.Helper()
	n, err := New(testConfig(), Deps{Store: st, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(n.Stop)
	return n
}

func vote(term uint64, candidate string) RequestVoteRequest {
	return RequestVoteRequest{
		ProtocolVersion: ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: candidate,
		Term: term, CandidateID: candidate,
	}
}

// --- tests ---

func TestNewNodeEmptyStore(t *testing.T) {
	n := noLoopNode(t, openStore(t))
	if n.role != RoleFollower {
		t.Errorf("role = %s, want follower", n.role)
	}
	if n.currentTerm != 0 || n.votedFor != "" {
		t.Errorf("fresh node term=%d vote=%q, want 0/empty", n.currentTerm, n.votedFor)
	}
	if n.lastLogIndex != 0 { // the index-0 sentinel
		t.Errorf("lastLogIndex = %d, want 0", n.lastLogIndex)
	}
}

func TestRecoveryFromStore(t *testing.T) {
	st := openStore(t)
	if err := st.SetTermAndVote(7, "node2"); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEntries([]LogEntry{
		{Index: 1, Term: 5, Kind: KindNoop},
		{Index: 2, Term: 7, Kind: KindCommand, Command: []byte("x")},
		{Index: 3, Term: 7, Kind: KindCommand, Command: []byte("y")},
	}); err != nil {
		t.Fatal(err)
	}
	// commit == applied so recovery does no replay (replay with a real state
	// machine is covered by the integration recovery test).
	if err := st.SetCommitIndex(1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastApplied(1); err != nil {
		t.Fatal(err)
	}

	n := noLoopNode(t, st)
	if n.currentTerm != 7 || n.votedFor != "node2" {
		t.Errorf("recovered term=%d vote=%q, want 7/node2", n.currentTerm, n.votedFor)
	}
	if n.lastLogIndex != 3 || n.lastLogTerm != 7 {
		t.Errorf("recovered lastLog=%d/%d, want 3/7", n.lastLogIndex, n.lastLogTerm)
	}
	if n.commitIndex != 1 || n.lastApplied != 1 {
		t.Errorf("recovered commit=%d applied=%d, want 1/1", n.commitIndex, n.lastApplied)
	}
}

func TestRecoveryReplaysCommittedUnapplied(t *testing.T) {
	st := openStore(t)
	if err := st.AppendEntries([]LogEntry{
		{Index: 1, Term: 1, Kind: KindNoop},
		{Index: 2, Term: 1, Kind: KindNoop},
		{Index: 3, Term: 1, Kind: KindNoop},
	}); err != nil {
		t.Fatal(err)
	}
	// Committed through 3 but only applied through 1: recovery must replay 2 and 3.
	if err := st.SetCommitIndex(3); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastApplied(1); err != nil {
		t.Fatal(err)
	}

	n := noLoopNode(t, st) // no ApplyFunc: applyEntry advances the checkpoint
	if n.lastApplied != 3 {
		t.Errorf("lastApplied after replay = %d, want 3", n.lastApplied)
	}
	if applied, _ := st.LastApplied(); applied != 3 {
		t.Errorf("persisted lastApplied = %d, want 3", applied)
	}
}

func TestBecomeFollowerHigherTermClearsVote(t *testing.T) {
	st := openStore(t)
	n := noLoopNode(t, st)
	if err := n.becomeCandidate(); err != nil { // term 1, votes for self
		t.Fatal(err)
	}
	if n.votedFor != "node1" {
		t.Fatalf("after candidacy vote = %q, want node1", n.votedFor)
	}
	if err := n.becomeFollower(5, "node3"); err != nil {
		t.Fatal(err)
	}
	if n.currentTerm != 5 || n.votedFor != "" || n.role != RoleFollower {
		t.Errorf("after step down term=%d vote=%q role=%s", n.currentTerm, n.votedFor, n.role)
	}
	// Persisted, not just in memory.
	if term, _ := st.CurrentTerm(); term != 5 {
		t.Errorf("persisted term = %d, want 5", term)
	}
	if v, _ := st.VotedFor(); v != "" {
		t.Errorf("persisted vote = %q, want cleared", v)
	}
}

func TestLegalTransitions(t *testing.T) {
	st := openStore(t)
	n := noLoopNode(t, st)

	if err := n.becomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if n.role != RoleCandidate || n.currentTerm != 1 || n.votedFor != "node1" {
		t.Errorf("candidate state: role=%s term=%d vote=%q", n.role, n.currentTerm, n.votedFor)
	}
	if term, _ := st.CurrentTerm(); term != 1 {
		t.Errorf("candidacy not persisted: term=%d", term)
	}

	n.becomeLeader()
	if n.role != RoleLeader || n.leaderID != "node1" {
		t.Errorf("leader state: role=%s leader=%q", n.role, n.leaderID)
	}
	for _, p := range n.peers {
		if n.nextIndex[p] != n.lastLogIndex+1 || n.matchIndex[p] != 0 {
			t.Errorf("peer %s progress next=%d match=%d", p, n.nextIndex[p], n.matchIndex[p])
		}
	}
}

func TestClusterIDMismatchRefused(t *testing.T) {
	st := openStore(t)
	// First node pins cluster id "test-cluster".
	if _, err := newNode(testConfig(), Deps{Store: st}); err != nil {
		t.Fatalf("first newNode: %v", err)
	}
	// A different configured cluster id on the same store must be refused.
	cfg := testConfig()
	cfg.ClusterID = "other-cluster"
	if _, err := newNode(cfg, Deps{Store: st}); err == nil {
		t.Fatal("expected cluster id mismatch error, got nil")
	}
}

func TestHandleRequestVoteHigherTermStepsDownAndGrants(t *testing.T) {
	st := openStore(t)
	n := runningNode(t, st)

	// A higher-term RequestVote from a candidate with an up-to-date log: the node
	// steps down (persisting the new term and clearing its old vote) and then
	// grants its vote for the new term.
	resp, err := n.HandleRequestVote(context.Background(), vote(5, "node2"))
	if err != nil {
		t.Fatalf("HandleRequestVote: %v", err)
	}
	if resp.Term != 5 || !resp.VoteGranted {
		t.Errorf("resp term=%d granted=%v, want 5/true", resp.Term, resp.VoteGranted)
	}
	if term, _ := st.CurrentTerm(); term != 5 {
		t.Errorf("persisted term = %d, want 5", term)
	}
	if v, _ := st.VotedFor(); v != "node2" {
		t.Errorf("persisted vote = %q, want node2", v)
	}
	s, _ := n.Status()
	if s.Term != 5 || s.Role != RoleFollower {
		t.Errorf("status term=%d role=%s", s.Term, s.Role)
	}
}

func TestOnlyOneVotePerTerm(t *testing.T) {
	st := openStore(t)
	n := runningNode(t, st)

	first, _ := n.HandleRequestVote(context.Background(), vote(5, "node2"))
	if !first.VoteGranted {
		t.Fatal("first vote should be granted")
	}
	// A different candidate in the same term must be denied.
	second, _ := n.HandleRequestVote(context.Background(), vote(5, "node3"))
	if second.VoteGranted {
		t.Error("second candidate in same term should be denied")
	}
}

func TestHandleAppendEntriesHigherTermRecognizesLeaderAndClearsVote(t *testing.T) {
	st := openStore(t)
	n := runningNode(t, st)

	// First cast a vote in term 5 so we can observe it being cleared.
	if _, err := n.HandleRequestVote(context.Background(), vote(5, "node2")); err != nil {
		t.Fatal(err)
	}

	// A higher-term AppendEntries recognizes the leader and clears the vote
	// (AppendEntries never grants a vote).
	req := AppendEntriesRequest{
		ProtocolVersion: ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node3",
		Term: 6, LeaderID: "node3",
	}
	resp, err := n.HandleAppendEntries(context.Background(), req)
	if err != nil {
		t.Fatalf("HandleAppendEntries: %v", err)
	}
	if resp.Term != 6 {
		t.Errorf("resp.Term = %d, want 6", resp.Term)
	}
	if v, _ := st.VotedFor(); v != "" {
		t.Errorf("vote = %q, want cleared after higher-term AppendEntries", v)
	}
	s, _ := n.Status()
	if s.Term != 6 || s.LeaderID != "node3" || s.Role != RoleFollower {
		t.Errorf("status term=%d leader=%q role=%s", s.Term, s.LeaderID, s.Role)
	}
}

func TestStaleTermRejected(t *testing.T) {
	st := openStore(t)
	n := runningNode(t, st)

	// Advance to term 5.
	if _, err := n.HandleRequestVote(context.Background(), vote(5, "node2")); err != nil {
		t.Fatal(err)
	}
	// A stale term-3 request must not change state.
	resp, err := n.HandleRequestVote(context.Background(), vote(3, "node3"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Term != 5 || resp.VoteGranted {
		t.Errorf("stale request resp term=%d granted=%v, want 5/false", resp.Term, resp.VoteGranted)
	}
	if term, _ := st.CurrentTerm(); term != 5 {
		t.Errorf("term changed to %d on stale request", term)
	}
}

func TestWrongClusterIDIgnored(t *testing.T) {
	st := openStore(t)
	n := runningNode(t, st)
	req := vote(9, "node2")
	req.ClusterID = "wrong"
	resp, err := n.HandleRequestVote(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Term != 0 { // request ignored, term unchanged
		t.Errorf("resp.Term = %d, want 0 (ignored)", resp.Term)
	}
	if term, _ := st.CurrentTerm(); term != 0 {
		t.Errorf("term advanced to %d on wrong-cluster request", term)
	}
}

func TestConcurrentStatusReads(t *testing.T) {
	st := openStore(t)
	n := runningNode(t, st)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := n.Status(); err != nil {
					t.Errorf("Status: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestStopReturnsErrStopped(t *testing.T) {
	st := openStore(t)
	n, err := New(testConfig(), Deps{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	n.Stop()

	if _, err := n.Status(); !errors.Is(err, ErrStopped) {
		t.Errorf("Status after stop = %v, want ErrStopped", err)
	}
	if _, err := n.HandleRequestVote(context.Background(), vote(3, "node2")); !errors.Is(err, ErrStopped) {
		t.Errorf("HandleRequestVote after stop = %v, want ErrStopped", err)
	}
}
