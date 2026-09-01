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

// handleRequestVote runs on the event loop. In this phase it enforces the term
// rules and higher-term step-down; granting a vote (log-freshness comparison,
// one vote per term, resetting the election timer) is added in Phase 5.
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
	return resp
}

// handleAppendEntries runs on the event loop. In this phase it enforces the term
// rules, recognizes the leader, and steps down; log matching, conflict repair,
// and commit advancement are added in Phase 6.
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
		return resp
	}
	// A valid leader at an equal or higher term makes this node a follower and
	// establishes the current leader.
	if err := n.becomeFollower(req.Term, req.LeaderID); err != nil {
		n.log.Error("step down on AppendEntries failed", "error", err)
		return resp
	}
	resp.Term = n.currentTerm
	return resp
}
