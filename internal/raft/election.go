package raft

import (
	"context"
	"time"
)

// voteResult carries a RequestVote response back to the event loop. term is the
// election term the request was sent in, used to ignore stale responses.
type voteResult struct {
	term uint64
	resp RequestVoteResponse
	err  error
}

// appendResult carries an AppendEntries response back to the event loop.
type appendResult struct {
	peer string
	term uint64
	resp AppendEntriesResponse
	err  error
}

// --- timer management (run-loop only) ---

func (n *Node) randomElectionTimeout() time.Duration {
	if n.electionMax <= n.electionMin {
		return n.electionMin
	}
	return n.electionMin + time.Duration(n.rng.Int63n(int64(n.electionMax-n.electionMin)))
}

func (n *Node) resetElectionTimer() { n.electionCh = n.clock.After(n.randomElectionTimeout()) }
func (n *Node) stopElectionTimer()  { n.electionCh = nil }
func (n *Node) startHeartbeat()     { n.heartbeatCh = n.clock.After(n.heartbeatInterval) }
func (n *Node) stopHeartbeat()      { n.heartbeatCh = nil }

// --- vote granting helpers ---

// persistVote records a granted vote before the node responds, so a crash cannot
// let it vote twice in one term.
func (n *Node) persistVote(candidateID string) error {
	if err := n.store.SetVotedFor(candidateID); err != nil {
		return err
	}
	n.votedFor = candidateID
	return nil
}

// candidateLogUpToDate reports whether a candidate's log is at least as
// up-to-date as this node's: a higher last term wins, otherwise a higher-or-equal
// last index.
func (n *Node) candidateLogUpToDate(lastLogTerm, lastLogIndex uint64) bool {
	if lastLogTerm != n.lastLogTerm {
		return lastLogTerm > n.lastLogTerm
	}
	return lastLogIndex >= n.lastLogIndex
}

// --- election ---

// onElectionTimeout starts a new election when a follower or candidate has not
// heard from a leader (or completed an election) in time.
func (n *Node) onElectionTimeout() {
	if n.role == RoleLeader {
		return
	}
	n.startElection()
}

// startElection increments the term, votes for self, and requests votes from all
// peers concurrently. becomeCandidate re-arms the election timer, so a failed
// election is retried automatically.
func (n *Node) startElection() {
	if err := n.becomeCandidate(); err != nil {
		n.log.Error("failed to start election", "error", err)
		n.resetElectionTimer()
		return
	}
	term := n.currentTerm
	n.votesGranted = 1 // vote for self
	if n.metrics != nil {
		n.metrics.IncElection()
	}

	req := RequestVoteRequest{
		ProtocolVersion: ProtocolVersion,
		ClusterID:       n.clusterID,
		SourceNodeID:    n.id,
		Term:            term,
		CandidateID:     n.id,
		LastLogIndex:    n.lastLogIndex,
		LastLogTerm:     n.lastLogTerm,
	}
	n.log.Info("starting election", "term", term)
	for _, peer := range n.peers {
		go n.sendRequestVote(peer, term, req)
	}
}

func (n *Node) sendRequestVote(peer string, term uint64, req RequestVoteRequest) {
	if n.transport == nil {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, n.rpcTimeout)
	defer cancel()
	resp, err := n.transport.SendRequestVote(ctx, peer, req)
	n.incRPC("request_vote", rpcResult(err))
	select {
	case n.voteRespCh <- voteResult{term: term, resp: resp, err: err}:
	case <-n.ctx.Done():
	}
}

// handleVoteResponse counts a vote or steps down on a higher term. Votes are
// counted only for the current election term and only while still a candidate.
func (n *Node) handleVoteResponse(vr voteResult) {
	if vr.err != nil {
		return
	}
	if vr.resp.Term > n.currentTerm {
		if err := n.becomeFollower(vr.resp.Term, ""); err != nil {
			n.log.Error("step down on vote response failed", "error", err)
		}
		return
	}
	if n.role != RoleCandidate || vr.term != n.currentTerm {
		return // stale response from an old election
	}
	if vr.resp.VoteGranted {
		n.votesGranted++
		if n.votesGranted >= n.quorum {
			n.promoteToLeader()
		}
	}
}

// promoteToLeader completes the transition to leader: it appends a no-op entry in
// the current term to assert leadership (and let the current-term commit rule
// carry forward entries from prior terms) and replicates immediately.
func (n *Node) promoteToLeader() {
	n.becomeLeader()
	noop := LogEntry{Index: n.lastLogIndex + 1, Term: n.currentTerm, Kind: KindNoop}
	if err := n.store.AppendEntries([]LogEntry{noop}); err != nil {
		n.log.Error("append leader no-op failed", "error", err)
	} else {
		n.lastLogIndex = noop.Index
		n.lastLogTerm = noop.Term
		n.nextIndex[n.id] = noop.Index + 1 // harmless; self is not in peers
	}
	n.log.Info("became leader", "term", n.currentTerm, "last_index", n.lastLogIndex)
	n.replicateToAll()
}

// onHeartbeat replicates to every peer on each tick, which both maintains
// leadership and retries lagging followers.
func (n *Node) onHeartbeat() {
	if n.role != RoleLeader {
		n.stopHeartbeat()
		return
	}
	n.replicateToAll()
	n.startHeartbeat()
}

func (n *Node) sendAppendEntries(peer string, term uint64, req AppendEntriesRequest) {
	if n.transport == nil {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, n.rpcTimeout)
	defer cancel()
	resp, err := n.transport.SendAppendEntries(ctx, peer, req)
	n.incRPC("append_entries", rpcResult(err))
	select {
	case n.appendRespCh <- appendResult{peer: peer, term: term, resp: resp, err: err}:
	case <-n.ctx.Done():
	}
}

func rpcResult(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
