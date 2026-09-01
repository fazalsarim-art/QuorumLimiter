package raft

import "context"

// HandleRequestVote processes an incoming RequestVote RPC by routing it through
// the event loop, so all state access stays single-threaded.
func (n *Node) HandleRequestVote(ctx context.Context, req RequestVoteRequest) (RequestVoteResponse, error) {
	env := voteEnvelope{req: req, resp: make(chan RequestVoteResponse, 1)}
	select {
	case n.voteCh <- env:
	case <-ctx.Done():
		return RequestVoteResponse{}, ctx.Err()
	case <-n.ctx.Done():
		return RequestVoteResponse{}, ErrStopped
	}
	select {
	case resp := <-env.resp:
		return resp, nil
	case <-ctx.Done():
		return RequestVoteResponse{}, ctx.Err()
	case <-n.ctx.Done():
		return RequestVoteResponse{}, ErrStopped
	}
}

// HandleAppendEntries processes an incoming AppendEntries RPC via the event loop.
func (n *Node) HandleAppendEntries(ctx context.Context, req AppendEntriesRequest) (AppendEntriesResponse, error) {
	env := appendEnvelope{req: req, resp: make(chan AppendEntriesResponse, 1)}
	select {
	case n.appendCh <- env:
	case <-ctx.Done():
		return AppendEntriesResponse{}, ctx.Err()
	case <-n.ctx.Done():
		return AppendEntriesResponse{}, ErrStopped
	}
	select {
	case resp := <-env.resp:
		return resp, nil
	case <-ctx.Done():
		return AppendEntriesResponse{}, ctx.Err()
	case <-n.ctx.Done():
		return AppendEntriesResponse{}, ErrStopped
	}
}

// handleRequestVote runs on the event loop. It enforces the term rules, steps
// down on a higher term, and grants at most one vote per term to a candidate
// whose log is at least as up-to-date. Granting a vote resets the election timer.
func (n *Node) handleRequestVote(req RequestVoteRequest) RequestVoteResponse {
	resp := RequestVoteResponse{
		ProtocolVersion: ProtocolVersion,
		SourceNodeID:    n.id,
		Term:            n.currentTerm,
		VoteGranted:     false,
	}
	if req.ProtocolVersion != ProtocolVersion || req.ClusterID != n.clusterID {
		return resp
	}
	if req.Term < n.currentTerm {
		return resp
	}
	if req.Term > n.currentTerm {
		if err := n.becomeFollower(req.Term, ""); err != nil {
			n.log.Error("step down on RequestVote failed", "error", err)
			return resp // keep old term; do not advance in memory past disk
		}
	}
	resp.Term = n.currentTerm

	alreadyVoted := n.votedFor != "" && n.votedFor != req.CandidateID
	if !alreadyVoted && n.candidateLogUpToDate(req.LastLogTerm, req.LastLogIndex) {
		if err := n.persistVote(req.CandidateID); err != nil {
			n.log.Error("persist vote failed", "error", err)
			return resp
		}
		resp.VoteGranted = true
		n.resetElectionTimer() // granting a vote defers our own election
	}
	return resp
}

// handleAppendEntries runs on the event loop. It enforces the term rules,
// recognizes the leader, checks log consistency at prevLogIndex (returning
// conflict hints on mismatch), transactionally repairs any conflicting suffix
// and appends new entries, and advances the commit index.
func (n *Node) handleAppendEntries(req AppendEntriesRequest) AppendEntriesResponse {
	resp := AppendEntriesResponse{
		ProtocolVersion: ProtocolVersion,
		SourceNodeID:    n.id,
		Term:            n.currentTerm,
		Success:         false,
	}
	if req.ProtocolVersion != ProtocolVersion || req.ClusterID != n.clusterID {
		return resp
	}
	if req.Term < n.currentTerm {
		return resp // stale leader
	}
	// A valid leader at an equal or higher term makes this node a follower,
	// establishes the current leader, and resets the election timer.
	if err := n.becomeFollower(req.Term, req.LeaderID); err != nil {
		n.log.Error("step down on AppendEntries failed", "error", err)
		return resp
	}
	resp.Term = n.currentTerm

	// Log consistency: the follower must contain prevLogIndex with prevLogTerm.
	if req.PrevLogIndex > n.lastLogIndex {
		// Follower's log is too short. Hint the leader to back up to our end.
		resp.ConflictTerm = 0
		resp.ConflictIndex = n.lastLogIndex + 1
		return resp
	}
	if req.PrevLogIndex > 0 { // index 0 is the sentinel and always matches
		entry, ok, err := n.store.Entry(req.PrevLogIndex)
		if err != nil {
			n.log.Error("read prev entry failed", "error", err)
			return resp
		}
		if !ok || entry.Term != req.PrevLogTerm {
			resp.ConflictTerm = entry.Term // 0 if the entry was missing
			resp.ConflictIndex = n.firstIndexOfTerm(req.PrevLogIndex, entry.Term)
			return resp
		}
	}

	// prevLog matches. Repair any conflicting suffix and append new entries in
	// one transaction.
	truncateFrom, toAppend, err := n.reconcileEntries(req.PrevLogIndex, req.Entries)
	if err != nil {
		n.log.Error("reconcile entries failed", "error", err)
		return resp
	}
	if truncateFrom > 0 || len(toAppend) > 0 {
		if err := n.store.OverwriteEntries(truncateFrom, toAppend); err != nil {
			n.log.Error("overwrite entries failed", "error", err)
			return resp
		}
		if err := n.refreshLastLog(); err != nil {
			n.log.Error("refresh last log failed", "error", err)
			return resp
		}
	}

	resp.Success = true
	resp.MatchIndex = req.PrevLogIndex + uint64(len(req.Entries))

	// Advance commit to what the leader reports, bounded by what we now hold.
	if req.LeaderCommit > n.commitIndex {
		newCommit := req.LeaderCommit
		if resp.MatchIndex < newCommit {
			newCommit = resp.MatchIndex
		}
		if newCommit > n.commitIndex {
			n.commitIndex = newCommit
			if err := n.store.SetCommitIndex(newCommit); err != nil {
				n.log.Error("persist commit index failed", "error", err)
			}
			n.applyCommitted()
		}
	}
	return resp
}
