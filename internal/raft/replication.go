package raft

import "sort"

// replicateToAll sends AppendEntries to every peer. It is called on each
// heartbeat tick and whenever a new proposal is appended.
func (n *Node) replicateToAll() {
	if n.role != RoleLeader {
		return
	}
	for _, peer := range n.peers {
		n.replicateToPeer(peer)
	}
}

// replicateToPeer builds one AppendEntries for peer from its nextIndex and sends
// it in a goroutine. The request is built here (in the loop) from mutable state;
// the goroutine only reads the immutable snapshot it is handed.
func (n *Node) replicateToPeer(peer string) {
	if n.role != RoleLeader {
		return
	}
	next := n.nextIndex[peer]
	if next < 1 {
		next = 1
	}
	prevIndex := next - 1
	prevTerm := uint64(0)
	if prevIndex > 0 {
		if e, ok, err := n.store.Entry(prevIndex); err == nil && ok {
			prevTerm = e.Term
		}
	}
	var entries []LogEntry
	if next <= n.lastLogIndex {
		es, err := n.store.Entries(next, n.lastLogIndex)
		if err != nil {
			n.log.Error("read entries for replication failed", "error", err)
			return
		}
		entries = es
	}
	req := AppendEntriesRequest{
		ProtocolVersion: ProtocolVersion,
		ClusterID:       n.clusterID,
		SourceNodeID:    n.id,
		Term:            n.currentTerm,
		LeaderID:        n.id,
		PrevLogIndex:    prevIndex,
		PrevLogTerm:     prevTerm,
		Entries:         entries,
		LeaderCommit:    n.commitIndex,
	}
	go n.sendAppendEntries(peer, n.currentTerm, req)
}

// handleAppendResponse processes a follower's AppendEntries reply on the leader:
// it steps down on a higher term, advances that follower's progress on success
// (then tries to advance commit), or backs up nextIndex on a log conflict and
// retries.
func (n *Node) handleAppendResponse(ar appendResult) {
	if ar.err != nil {
		return
	}
	if ar.resp.Term > n.currentTerm {
		if err := n.becomeFollower(ar.resp.Term, ""); err != nil {
			n.log.Error("step down on append response failed", "error", err)
		}
		return
	}
	// Ignore stale responses: only act while still leader in the same term the
	// request was sent.
	if n.role != RoleLeader || ar.term != n.currentTerm {
		return
	}
	// A reply (success or a benign conflict) is proof of contact with this peer.
	n.peerContactMS[ar.peer] = n.nowMS()
	if ar.resp.Success {
		if ar.resp.MatchIndex > n.matchIndex[ar.peer] {
			n.matchIndex[ar.peer] = ar.resp.MatchIndex
			n.nextIndex[ar.peer] = ar.resp.MatchIndex + 1
		}
		n.advanceCommit()
		return
	}
	// Log mismatch: back up and retry.
	n.backupNextIndex(ar.peer, ar.resp)
	n.replicateToPeer(ar.peer)
}

// backupNextIndex applies the conflict hints to move a follower's nextIndex
// backward efficiently.
func (n *Node) backupNextIndex(peer string, resp AppendEntriesResponse) {
	var next uint64
	switch {
	case resp.ConflictTerm == 0:
		// Follower's log was too short (or empty at prevLogIndex).
		next = resp.ConflictIndex
	default:
		if idx := n.lastIndexWithTerm(resp.ConflictTerm); idx > 0 {
			next = idx + 1
		} else {
			next = resp.ConflictIndex
		}
	}
	if next < 1 {
		next = 1
	}
	n.nextIndex[peer] = next
}

// advanceCommit advances the commit index to the highest index replicated on a
// majority, subject to the current-term rule: an entry is only committed by count
// if it belongs to the leader's current term.
func (n *Node) advanceCommit() {
	// Match indexes across the cluster, including this leader's own last index.
	matches := make([]uint64, 0, len(n.peers)+1)
	matches = append(matches, n.lastLogIndex)
	for _, p := range n.peers {
		matches = append(matches, n.matchIndex[p])
	}
	// Sort descending; the (quorum-1)th element is the highest index present on a
	// majority.
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	candidate := matches[n.quorum-1]
	if candidate <= n.commitIndex {
		return
	}
	entry, ok, err := n.store.Entry(candidate)
	if err != nil || !ok {
		return
	}
	if entry.Term != n.currentTerm {
		// Current-term rule: do not commit an older-term entry by majority count
		// alone. It becomes committed once a current-term entry above it does.
		return
	}
	n.commitIndex = candidate
	if err := n.store.SetCommitIndex(candidate); err != nil {
		n.log.Error("persist commit index failed", "error", err)
	}
	n.applyCommitted()
	// Broadcast the new commit index promptly so followers apply too.
	n.replicateToAll()
}

// firstIndexOfTerm returns the first index at or below `from` whose entry has the
// given term, used to build a follower's conflict hint. If term is 0 (missing
// entry) it returns `from`.
func (n *Node) firstIndexOfTerm(from, term uint64) uint64 {
	if term == 0 {
		return from
	}
	idx := from
	for idx > 1 {
		e, ok, err := n.store.Entry(idx - 1)
		if err != nil || !ok || e.Term != term {
			break
		}
		idx--
	}
	return idx
}

// lastIndexWithTerm returns the highest index in this node's log whose entry has
// the given term, or 0 if none.
func (n *Node) lastIndexWithTerm(term uint64) uint64 {
	for idx := n.lastLogIndex; idx >= 1; idx-- {
		e, ok, err := n.store.Entry(idx)
		if err != nil || !ok {
			return 0
		}
		if e.Term == term {
			return idx
		}
	}
	return 0
}

// reconcileEntries determines how to merge incoming entries with the follower's
// existing log: it returns the index to truncate from (0 if none) and the slice
// of entries to append. Entries already present with matching terms are skipped;
// the first term conflict triggers truncation from that index.
func (n *Node) reconcileEntries(prevIndex uint64, entries []LogEntry) (truncateFrom uint64, toAppend []LogEntry, err error) {
	for i := range entries {
		idx := prevIndex + 1 + uint64(i)
		existing, ok, eerr := n.store.Entry(idx)
		if eerr != nil {
			return 0, nil, eerr
		}
		if !ok {
			return 0, entries[i:], nil // nothing here yet; append the rest
		}
		if existing.Term != entries[i].Term {
			return idx, entries[i:], nil // conflict; truncate from here, append rest
		}
	}
	return 0, nil, nil // every incoming entry is already present and matching
}

// refreshLastLog reloads lastLogIndex/lastLogTerm from the store after a log
// mutation.
func (n *Node) refreshLastLog() error {
	last, err := n.store.LastIndex()
	if err != nil {
		return err
	}
	term := uint64(0)
	if e, ok, eerr := n.store.Entry(last); eerr != nil {
		return eerr
	} else if ok {
		term = e.Term
	}
	n.lastLogIndex = last
	n.lastLogTerm = term
	return nil
}
