// Package testcluster provides a reusable in-process multi-node QuorumLimiter
// cluster for integration tests: real storage and state machines behind an
// in-memory transport whose directed links can be cut to simulate partitions.
package testcluster

import (
	"context"
	"errors"
	"sync"

	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

// rpcTarget is the subset of *raft.Node the transport routes to.
type rpcTarget interface {
	HandleRequestVote(ctx context.Context, req raft.RequestVoteRequest) (raft.RequestVoteResponse, error)
	HandleAppendEntries(ctx context.Context, req raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error)
}

// ErrPartitioned is returned when a directed link is cut.
var ErrPartitioned = errors.New("testcluster: link partitioned")

// FaultTransport routes RPCs between in-process nodes and can block directed
// links to simulate (possibly asymmetric) network partitions.
type FaultTransport struct {
	mu      sync.Mutex
	nodes   map[string]rpcTarget
	blocked map[string]map[string]bool // blocked[from][to]
}

// NewFaultTransport creates an empty transport.
func NewFaultTransport() *FaultTransport {
	return &FaultTransport{
		nodes:   make(map[string]rpcTarget),
		blocked: make(map[string]map[string]bool),
	}
}

func (ft *FaultTransport) register(id string, node rpcTarget) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.nodes[id] = node
}

// Block cuts the directed link from -> to.
func (ft *FaultTransport) Block(from, to string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.blocked[from] == nil {
		ft.blocked[from] = make(map[string]bool)
	}
	ft.blocked[from][to] = true
}

// Unblock restores the directed link from -> to.
func (ft *FaultTransport) Unblock(from, to string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.blocked[from] != nil {
		delete(ft.blocked[from], to)
	}
}

func (ft *FaultTransport) lookup(from, to string) (rpcTarget, bool) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.blocked[from][to] {
		return nil, false
	}
	n, ok := ft.nodes[to]
	return n, ok
}

// SendRequestVote implements raft.Transport.
func (ft *FaultTransport) SendRequestVote(ctx context.Context, target string, req raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	n, ok := ft.lookup(req.SourceNodeID, target)
	if !ok {
		return raft.RequestVoteResponse{}, ErrPartitioned
	}
	return n.HandleRequestVote(ctx, req)
}

// SendAppendEntries implements raft.Transport.
func (ft *FaultTransport) SendAppendEntries(ctx context.Context, target string, req raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	n, ok := ft.lookup(req.SourceNodeID, target)
	if !ok {
		return raft.AppendEntriesResponse{}, ErrPartitioned
	}
	return n.HandleAppendEntries(ctx, req)
}
