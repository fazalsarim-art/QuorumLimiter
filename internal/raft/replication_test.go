package raft

import "testing"

// leaderNode returns a no-loop node forced into leader state at the given term,
// with fresh next/match index maps, for deterministic replication unit tests.
func leaderNode(t *testing.T, term uint64) *Node {
	t.Helper()
	n := noLoopNode(t, openStore(t))
	n.role = RoleLeader
	n.currentTerm = term
	n.nextIndex = make(map[string]uint64)
	n.matchIndex = make(map[string]uint64)
	for _, p := range n.peers {
		n.nextIndex[p] = 1
		n.matchIndex[p] = 0
	}
	return n
}

func TestCommitCurrentTermRule(t *testing.T) {
	n := leaderNode(t, 2)
	// Log: index 1 (old term 1), index 2 (current term 2).
	if err := n.store.AppendEntries([]LogEntry{
		{Index: 1, Term: 1, Kind: KindCommand, Command: []byte("a")},
		{Index: 2, Term: 2, Kind: KindNoop},
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.refreshLastLog(); err != nil {
		t.Fatal(err)
	}

	// A majority holds index 1, but it is from an OLD term: must not commit.
	n.matchIndex["node2"] = 1
	n.matchIndex["node3"] = 0
	n.advanceCommit()
	if n.commitIndex != 0 {
		t.Fatalf("commitIndex = %d, want 0 (old-term entry must not commit by count)", n.commitIndex)
	}

	// Once a majority holds index 2 (current term), it commits — and index 1
	// commits transitively.
	n.matchIndex["node2"] = 2
	n.advanceCommit()
	if n.commitIndex != 2 {
		t.Fatalf("commitIndex = %d, want 2", n.commitIndex)
	}
}

func TestConflictHintFollowerTooShort(t *testing.T) {
	n := noLoopNode(t, openStore(t)) // log holds only the index-0 sentinel

	resp := n.handleAppendEntries(AppendEntriesRequest{
		ProtocolVersion: ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node2",
		Term: 1, LeaderID: "node2", PrevLogIndex: 5, PrevLogTerm: 1,
	})
	if resp.Success {
		t.Fatal("expected failure for too-short follower log")
	}
	if resp.ConflictTerm != 0 || resp.ConflictIndex != 1 {
		t.Errorf("conflict hint = term %d index %d, want 0/1", resp.ConflictTerm, resp.ConflictIndex)
	}
}

func TestConflictHintTermMismatch(t *testing.T) {
	n := noLoopNode(t, openStore(t))
	if err := n.store.AppendEntries([]LogEntry{
		{Index: 1, Term: 1, Kind: KindNoop},
		{Index: 2, Term: 2, Kind: KindNoop},
		{Index: 3, Term: 2, Kind: KindNoop},
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.refreshLastLog(); err != nil {
		t.Fatal(err)
	}

	// prevLogIndex 3 exists with term 2, but the leader claims term 5.
	resp := n.handleAppendEntries(AppendEntriesRequest{
		ProtocolVersion: ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node2",
		Term: 5, LeaderID: "node2", PrevLogIndex: 3, PrevLogTerm: 5,
	})
	if resp.Success {
		t.Fatal("expected failure on term mismatch")
	}
	if resp.ConflictTerm != 2 || resp.ConflictIndex != 2 {
		t.Errorf("conflict hint = term %d index %d, want 2/2 (first index of term 2)", resp.ConflictTerm, resp.ConflictIndex)
	}
}

func TestFollowerRepairsConflictingSuffix(t *testing.T) {
	n := noLoopNode(t, openStore(t))
	// Follower's (stale) log.
	if err := n.store.AppendEntries([]LogEntry{
		{Index: 1, Term: 1, Kind: KindNoop},
		{Index: 2, Term: 1, Kind: KindCommand, Command: []byte("stale2")},
		{Index: 3, Term: 1, Kind: KindCommand, Command: []byte("stale3")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := n.refreshLastLog(); err != nil {
		t.Fatal(err)
	}

	// Leader is authoritative from index 2 with term 2 (conflict at index 2).
	resp := n.handleAppendEntries(AppendEntriesRequest{
		ProtocolVersion: ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node2",
		Term: 2, LeaderID: "node2", PrevLogIndex: 1, PrevLogTerm: 1,
		Entries: []LogEntry{
			{Index: 2, Term: 2, Kind: KindCommand, Command: []byte("good2")},
			{Index: 3, Term: 2, Kind: KindCommand, Command: []byte("good3")},
		},
	})
	if !resp.Success {
		t.Fatalf("expected success repairing suffix")
	}
	if resp.MatchIndex != 3 {
		t.Errorf("matchIndex = %d, want 3", resp.MatchIndex)
	}
	e2, _, _ := n.store.Entry(2)
	e3, _, _ := n.store.Entry(3)
	if e2.Term != 2 || string(e2.Command) != "good2" || e3.Term != 2 || string(e3.Command) != "good3" {
		t.Errorf("suffix not repaired: e2=%+v e3=%+v", e2, e3)
	}
	if n.lastLogIndex != 3 || n.lastLogTerm != 2 {
		t.Errorf("lastLog = %d/%d, want 3/2", n.lastLogIndex, n.lastLogTerm)
	}
}

func TestFollowerAppliesUpToLeaderCommit(t *testing.T) {
	n := noLoopNode(t, openStore(t)) // nil ApplyFunc: applyEntry advances the checkpoint

	resp := n.handleAppendEntries(AppendEntriesRequest{
		ProtocolVersion: ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node2",
		Term: 1, LeaderID: "node2", PrevLogIndex: 0, PrevLogTerm: 0,
		Entries: []LogEntry{
			{Index: 1, Term: 1, Kind: KindNoop},
			{Index: 2, Term: 1, Kind: KindNoop},
		},
		LeaderCommit: 2,
	})
	if !resp.Success {
		t.Fatal("expected success")
	}
	if n.commitIndex != 2 {
		t.Errorf("commitIndex = %d, want 2", n.commitIndex)
	}
	if n.lastApplied != 2 {
		t.Errorf("lastApplied = %d, want 2 (should apply up to commit)", n.lastApplied)
	}
}
