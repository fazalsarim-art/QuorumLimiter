package raft

import "fmt"

// applyCommitted applies every entry from lastApplied+1 through commitIndex, in
// order, delivering each result to a waiting proposer if one is registered. It
// runs on the event loop.
func (n *Node) applyCommitted() {
	for n.lastApplied < n.commitIndex {
		idx := n.lastApplied + 1
		entry, ok, err := n.store.Entry(idx)
		if err != nil {
			n.log.Error("read entry for apply failed", "index", idx, "error", err)
			return // stop; retried on next commit advance
		}
		if !ok {
			n.log.Error("committed entry missing", "index", idx)
			return
		}
		result, aerr := n.applyEntry(idx, entry.Term, entry)
		if aerr != nil {
			n.log.Error("apply entry failed", "index", idx, "error", aerr)
			return // do not advance lastApplied; retry later
		}
		n.lastApplied = idx
		if w, ok := n.waiters[idx]; ok {
			w.ch <- applyOutcome{result: result}
			delete(n.waiters, idx)
		}
	}
}

// replayCommitted applies committed-but-unapplied entries during recovery,
// before the node serves any request. It runs single-threaded in newNode.
func (n *Node) replayCommitted() error {
	for n.lastApplied < n.commitIndex {
		idx := n.lastApplied + 1
		entry, ok, err := n.store.Entry(idx)
		if err != nil {
			return fmt.Errorf("raft: replay read entry %d: %w", idx, err)
		}
		if !ok {
			return fmt.Errorf("raft: committed entry %d missing during replay", idx)
		}
		if _, err := n.applyEntry(idx, entry.Term, entry); err != nil {
			return fmt.Errorf("raft: replay apply entry %d: %w", idx, err)
		}
		n.lastApplied = idx
	}
	return nil
}

// applyEntry applies one entry via the injected ApplyFunc, which advances
// last_applied atomically with the entry's effects. When no ApplyFunc is wired
// (raft-only tests), it just advances the applied checkpoint so the log makes
// progress.
func (n *Node) applyEntry(index, term uint64, entry LogEntry) (any, error) {
	if n.apply == nil {
		return nil, n.store.SetLastApplied(index)
	}
	return n.apply(index, term, entry)
}
