package raft

import "context"

// applyOutcome is delivered to a proposer once its entry is committed and applied.
type applyOutcome struct {
	result any
	err    error
}

// waiter is a registered proposal awaiting its entry's apply result.
type waiter struct {
	ch      chan applyOutcome
	startMS int64
}

// proposeEnvelope carries a proposal into the event loop and returns the
// registration (index + result channel, or an error such as not-leader).
type proposeEnvelope struct {
	command []byte
	resp    chan proposeRegistration
}

type proposeRegistration struct {
	index uint64
	ch    chan applyOutcome
	err   error
}

// Propose submits a command to be replicated and applied. It returns the state
// machine's result only after the entry is committed and applied. A caller
// timeout (ctx) returns ErrProposalTimeout but does NOT cancel the entry, which
// may still commit later; the caller should retry with the same idempotency key.
func (n *Node) Propose(ctx context.Context, command []byte) (any, error) {
	env := proposeEnvelope{command: command, resp: make(chan proposeRegistration, 1)}
	select {
	case n.proposeCh <- env:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.ctx.Done():
		return nil, ErrStopped
	}

	var reg proposeRegistration
	select {
	case reg = <-env.resp:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.ctx.Done():
		return nil, ErrStopped
	}
	if reg.err != nil {
		return nil, reg.err
	}

	select {
	case outcome := <-reg.ch:
		return outcome.result, outcome.err
	case <-ctx.Done():
		// Stop waiting, but leave the entry: it may still commit and apply. The
		// result is then retrievable via the same idempotency key.
		n.deregisterWaiter(reg.index)
		return nil, ErrProposalTimeout
	case <-n.ctx.Done():
		return nil, ErrStopped
	}
}

// handlePropose runs on the event loop: it appends the command locally, registers
// a waiter, and triggers replication. Proposals are accepted only on the leader.
func (n *Node) handlePropose(env proposeEnvelope) {
	if n.role != RoleLeader {
		env.resp <- proposeRegistration{err: ErrNotLeader}
		return
	}
	index := n.lastLogIndex + 1
	entry := LogEntry{Index: index, Term: n.currentTerm, Kind: KindCommand, Command: env.command}
	if err := n.store.AppendEntries([]LogEntry{entry}); err != nil {
		env.resp <- proposeRegistration{err: err}
		return
	}
	n.lastLogIndex = index
	n.lastLogTerm = n.currentTerm

	w := &waiter{ch: make(chan applyOutcome, 1), startMS: n.nowMS()}
	n.waiters[index] = w
	env.resp <- proposeRegistration{index: index, ch: w.ch}

	n.replicateToAll()
}

// deregisterWaiter asks the loop to drop a waiter (used on caller timeout).
func (n *Node) deregisterWaiter(index uint64) {
	select {
	case n.deregisterCh <- index:
	case <-n.ctx.Done():
	}
}

// failWaiters completes all pending waiters with err and clears the registry.
// Called on step-down: uncommitted proposals may be overwritten by a new leader.
func (n *Node) failWaiters(err error) {
	for idx, w := range n.waiters {
		w.ch <- applyOutcome{err: err}
		delete(n.waiters, idx)
	}
}

// LeaderID returns the node's currently known leader ID (may be empty). Useful
// for forwarding decisions to the leader.
func (n *Node) LeaderID() (string, error) {
	s, err := n.Status()
	if err != nil {
		return "", err
	}
	return s.LeaderID, nil
}
