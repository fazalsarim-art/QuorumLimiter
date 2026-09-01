package raft

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"
)

// Default election and heartbeat timing, used when a Config leaves them zero.
const (
	defaultElectionMin = 800 * time.Millisecond
	defaultElectionMax = 1400 * time.Millisecond
	defaultHeartbeat   = 200 * time.Millisecond
)

// Typed errors returned across the node boundary.
var (
	ErrStopped         = errors.New("raft: node stopped")
	ErrNotLeader       = errors.New("raft: not leader")
	ErrNoLeader        = errors.New("raft: no leader")
	ErrProposalTimeout = errors.New("raft: proposal timeout")
	ErrLostQuorum      = errors.New("raft: lost quorum")
)

// Store is the narrow persistence interface the node depends on; *storage.Store
// satisfies it. All methods perform short, self-contained transactions.
type Store interface {
	CurrentTerm() (uint64, error)
	VotedFor() (string, error)
	SetCurrentTerm(term uint64) error
	SetVotedFor(nodeID string) error
	SetTermAndVote(term uint64, votedFor string) error
	FirstIndex() (uint64, error)
	LastIndex() (uint64, error)
	Entry(index uint64) (LogEntry, bool, error)
	Entries(lo, hi uint64) ([]LogEntry, error)
	AppendEntries(entries []LogEntry) error
	TruncateSuffix(from uint64) error
	CommitIndex() (uint64, error)
	SetCommitIndex(index uint64) error
	LastApplied() (uint64, error)
	SetLastApplied(index uint64) error
	ClusterID() (string, error)
	SetClusterID(id string) error
}

// Transport sends RPCs to peers. Used from Phase 5 onward; the node never calls
// it while holding a store transaction.
type Transport interface {
	SendRequestVote(ctx context.Context, target string, req RequestVoteRequest) (RequestVoteResponse, error)
	SendAppendEntries(ctx context.Context, target string, req AppendEntriesRequest) (AppendEntriesResponse, error)
}

// ApplyFunc applies a committed entry to the state machine (wired in Phase 6).
type ApplyFunc func(entry LogEntry) error

// Clock abstracts time so tests can drive election timing deterministically.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config is the static configuration for a node.
type Config struct {
	NodeID    string
	ClusterID string
	Peers     []string // all node IDs, including this node

	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration
}

// Deps are the injectable dependencies for a node.
type Deps struct {
	Store     Store
	Transport Transport
	Apply     ApplyFunc
	Clock     Clock
	Logger    *slog.Logger
}

// envelopes carry an RPC plus a buffered channel for its response, so the event
// loop owns all mutable state and callers never touch it directly.
type voteEnvelope struct {
	req  RequestVoteRequest
	resp chan RequestVoteResponse
}

type appendEnvelope struct {
	req  AppendEntriesRequest
	resp chan AppendEntriesResponse
}

// Node is a single Raft member. All mutable consensus fields are owned by the
// run loop goroutine and are only read or written there.
type Node struct {
	id        string
	clusterID string
	peers     []string // other nodes (excluding self)
	quorum    int

	store     Store
	transport Transport
	apply     ApplyFunc
	clock     Clock
	log       *slog.Logger

	// timing
	electionMin       time.Duration
	electionMax       time.Duration
	heartbeatInterval time.Duration
	rpcTimeout        time.Duration
	rng               *rand.Rand

	// channels into the event loop
	voteCh       chan voteEnvelope
	appendCh     chan appendEnvelope
	statusCh     chan chan Status
	voteRespCh   chan voteResult
	appendRespCh chan appendResult

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// --- mutable state, owned by the run loop ---
	role         Role
	currentTerm  uint64
	votedFor     string
	leaderID     string
	commitIndex  uint64
	lastApplied  uint64
	lastLogIndex uint64
	lastLogTerm  uint64
	nextIndex    map[string]uint64
	matchIndex   map[string]uint64
	votesGranted int

	// timer channels; nil disables the corresponding timer for the current role.
	electionCh  <-chan time.Time
	heartbeatCh <-chan time.Time
}

// New constructs a node, recovers persisted state, and starts its event loop.
func New(cfg Config, deps Deps) (*Node, error) {
	n, err := newNode(cfg, deps)
	if err != nil {
		return nil, err
	}
	n.start()
	return n, nil
}

// start launches the event loop. It is separate from newNode so tests can build
// several nodes, register them with a shared transport, and then start them all.
func (n *Node) start() { go n.run() }

// newNode builds and recovers a node without starting the event loop. It is used
// directly by tests that exercise transition helpers single-threaded.
func newNode(cfg Config, deps Deps) (*Node, error) {
	if deps.Store == nil {
		return nil, errors.New("raft: Store is required")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("raft: NodeID is required")
	}
	clock := deps.Clock
	if clock == nil {
		clock = realClock{}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	others := make([]string, 0, len(cfg.Peers))
	for _, p := range cfg.Peers {
		if p != cfg.NodeID {
			others = append(others, p)
		}
	}

	electionMin := cfg.ElectionTimeoutMin
	if electionMin <= 0 {
		electionMin = defaultElectionMin
	}
	electionMax := cfg.ElectionTimeoutMax
	if electionMax <= electionMin {
		electionMax = electionMin + defaultElectionMax - defaultElectionMin
	}
	heartbeat := cfg.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}

	// Seed each node's election-timeout RNG independently so nodes do not all
	// time out together and split the vote.
	seed := time.Now().UnixNano()
	for _, c := range cfg.NodeID {
		seed = seed*31 + int64(c)
	}

	n := &Node{
		id:                cfg.NodeID,
		clusterID:         cfg.ClusterID,
		peers:             others,
		quorum:            len(cfg.Peers)/2 + 1,
		store:             deps.Store,
		transport:         deps.Transport,
		apply:             deps.Apply,
		clock:             clock,
		log:               logger.With(slog.String("component", "raft")),
		electionMin:       electionMin,
		electionMax:       electionMax,
		heartbeatInterval: heartbeat,
		rpcTimeout:        electionMin,
		rng:               rand.New(rand.NewSource(seed)),
		voteCh:            make(chan voteEnvelope),
		appendCh:          make(chan appendEnvelope),
		statusCh:          make(chan chan Status),
		voteRespCh:        make(chan voteResult),
		appendRespCh:      make(chan appendResult),
		done:              make(chan struct{}),
		role:              RoleFollower,
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())

	if err := n.recover(); err != nil {
		n.cancel()
		return nil, err
	}
	return n, nil
}

// recover loads persisted term, vote, log bounds, and checkpoints, and pins the
// cluster identity.
func (n *Node) recover() error {
	if err := n.recoverClusterID(); err != nil {
		return err
	}

	term, err := n.store.CurrentTerm()
	if err != nil {
		return fmt.Errorf("raft: recover term: %w", err)
	}
	vote, err := n.store.VotedFor()
	if err != nil {
		return fmt.Errorf("raft: recover vote: %w", err)
	}
	lastIdx, err := n.store.LastIndex()
	if err != nil {
		return fmt.Errorf("raft: recover last index: %w", err)
	}
	lastTerm := uint64(0)
	if entry, ok, eerr := n.store.Entry(lastIdx); eerr != nil {
		return fmt.Errorf("raft: recover last entry: %w", eerr)
	} else if ok {
		lastTerm = entry.Term
	}
	commit, err := n.store.CommitIndex()
	if err != nil {
		return fmt.Errorf("raft: recover commit index: %w", err)
	}
	applied, err := n.store.LastApplied()
	if err != nil {
		return fmt.Errorf("raft: recover last applied: %w", err)
	}

	n.currentTerm = term
	n.votedFor = vote
	n.lastLogIndex = lastIdx
	n.lastLogTerm = lastTerm
	n.commitIndex = commit
	n.lastApplied = applied
	n.role = RoleFollower
	n.leaderID = ""
	return nil
}

func (n *Node) recoverClusterID() error {
	stored, err := n.store.ClusterID()
	if err != nil {
		return fmt.Errorf("raft: recover cluster id: %w", err)
	}
	switch {
	case stored == "":
		if n.clusterID == "" {
			return errors.New("raft: cluster id is required")
		}
		if serr := n.store.SetClusterID(n.clusterID); serr != nil {
			return fmt.Errorf("raft: persist cluster id: %w", serr)
		}
	case n.clusterID != "" && stored != n.clusterID:
		return fmt.Errorf("raft: stored cluster id %q does not match configured %q", stored, n.clusterID)
	default:
		// Adopt the stored identity if none was configured.
		n.clusterID = stored
	}
	return nil
}

// run is the single event loop. It owns all mutable consensus state. The timer
// channels (electionCh, heartbeatCh) are re-read each iteration, so reassigning
// them from a handler changes what the next select waits on; a nil channel
// disables its case.
func (n *Node) run() {
	defer close(n.done)
	n.resetElectionTimer() // arm the follower election timer at startup
	for {
		select {
		case <-n.ctx.Done():
			return
		case env := <-n.voteCh:
			env.resp <- n.handleRequestVote(env.req)
		case env := <-n.appendCh:
			env.resp <- n.handleAppendEntries(env.req)
		case reply := <-n.statusCh:
			reply <- n.snapshotStatus()
		case <-n.electionCh:
			n.onElectionTimeout()
		case <-n.heartbeatCh:
			n.onHeartbeat()
		case vr := <-n.voteRespCh:
			n.handleVoteResponse(vr)
		case ar := <-n.appendRespCh:
			n.handleAppendResponse(ar)
		}
	}
}

// Stop cancels the event loop and waits for it to exit.
func (n *Node) Stop() {
	n.cancel()
	<-n.done
}

// Status returns an immutable snapshot of the node's consensus state.
func (n *Node) Status() (Status, error) {
	reply := make(chan Status, 1)
	select {
	case n.statusCh <- reply:
	case <-n.ctx.Done():
		return Status{}, ErrStopped
	}
	select {
	case s := <-reply:
		return s, nil
	case <-n.ctx.Done():
		return Status{}, ErrStopped
	}
}

func (n *Node) snapshotStatus() Status {
	s := Status{
		NodeID:       n.id,
		Role:         n.role,
		Term:         n.currentTerm,
		LeaderID:     n.leaderID,
		LastLogIndex: n.lastLogIndex,
		LastLogTerm:  n.lastLogTerm,
		CommitIndex:  n.commitIndex,
		LastApplied:  n.lastApplied,
	}
	if n.role == RoleLeader {
		for _, p := range n.peers {
			s.Peers = append(s.Peers, PeerProgress{
				NodeID:     p,
				NextIndex:  n.nextIndex[p],
				MatchIndex: n.matchIndex[p],
			})
		}
	}
	return s
}

// --- state transitions (run-loop only) ---

// becomeFollower steps down to follower. When the term increases it persists the
// new term and a cleared vote BEFORE any state-dependent response, which
// prevents voting twice in one term across a crash.
func (n *Node) becomeFollower(term uint64, leaderID string) error {
	if term > n.currentTerm {
		if err := n.store.SetTermAndVote(term, ""); err != nil {
			return fmt.Errorf("raft: persist term %d: %w", term, err)
		}
		n.currentTerm = term
		n.votedFor = ""
	}
	n.role = RoleFollower
	n.leaderID = leaderID
	n.stopHeartbeat()
	n.resetElectionTimer()
	return nil
}

// becomeCandidate begins a new election term, voting for self. It persists both
// the incremented term and the self-vote before the caller sends RequestVote
// (the sending side is implemented in Phase 5).
func (n *Node) becomeCandidate() error {
	newTerm := n.currentTerm + 1
	if err := n.store.SetTermAndVote(newTerm, n.id); err != nil {
		return fmt.Errorf("raft: persist candidacy term %d: %w", newTerm, err)
	}
	n.currentTerm = newTerm
	n.votedFor = n.id
	n.role = RoleCandidate
	n.leaderID = ""
	n.stopHeartbeat()
	n.resetElectionTimer()
	return nil
}

// becomeLeader initializes per-follower replication progress and switches timers
// from election to heartbeat. Appending the new-term no-op and sending the first
// heartbeat is done by promoteToLeader.
func (n *Node) becomeLeader() {
	n.role = RoleLeader
	n.leaderID = n.id
	n.nextIndex = make(map[string]uint64, len(n.peers))
	n.matchIndex = make(map[string]uint64, len(n.peers))
	for _, p := range n.peers {
		n.nextIndex[p] = n.lastLogIndex + 1
		n.matchIndex[p] = 0
	}
	n.stopElectionTimer()
	n.startHeartbeat()
}
