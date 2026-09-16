package raft

import (
	"fmt"

	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

// ProtocolVersion is the wire protocol version for internal RPCs. A mismatch is
// rejected so incompatible binaries cannot corrupt the log.
const ProtocolVersion = 1

// Role is a node's current Raft role.
type Role uint8

const (
	RoleFollower Role = iota
	RoleCandidate
	RoleLeader
)

func (r Role) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RoleCandidate:
		return "candidate"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// MarshalJSON renders the role as a readable string (e.g. "leader") in status
// responses rather than a numeric code.
func (r Role) MarshalJSON() ([]byte, error) {
	return []byte(`"` + r.String() + `"`), nil
}

// UnmarshalJSON parses the string form back into a Role.
func (r *Role) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"follower"`:
		*r = RoleFollower
	case `"candidate"`:
		*r = RoleCandidate
	case `"leader"`:
		*r = RoleLeader
	default:
		return fmt.Errorf("raft: unknown role %s", b)
	}
	return nil
}

// LogEntry is the persisted log entry type, owned by the storage layer. Raft
// aliases it so it reads naturally here without duplicating the definition.
type LogEntry = storage.LogEntry

// Log entry kinds (re-exported from storage for convenience).
const (
	KindSentinel = storage.KindSentinel
	KindNoop     = storage.KindNoop
	KindCommand  = storage.KindCommand
)

// RequestVoteRequest is sent by a candidate seeking a vote.
type RequestVoteRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	ClusterID       string `json:"cluster_id"`
	SourceNodeID    string `json:"source_node_id"`
	Term            uint64 `json:"term"`
	CandidateID     string `json:"candidate_id"`
	LastLogIndex    uint64 `json:"last_log_index"`
	LastLogTerm     uint64 `json:"last_log_term"`
}

// RequestVoteResponse answers a RequestVoteRequest.
type RequestVoteResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	SourceNodeID    string `json:"source_node_id"`
	Term            uint64 `json:"term"`
	VoteGranted     bool   `json:"vote_granted"`
}

// AppendEntriesRequest replicates entries and serves as a heartbeat.
type AppendEntriesRequest struct {
	ProtocolVersion int        `json:"protocol_version"`
	ClusterID       string     `json:"cluster_id"`
	SourceNodeID    string     `json:"source_node_id"`
	Term            uint64     `json:"term"`
	LeaderID        string     `json:"leader_id"`
	PrevLogIndex    uint64     `json:"prev_log_index"`
	PrevLogTerm     uint64     `json:"prev_log_term"`
	Entries         []LogEntry `json:"entries"`
	LeaderCommit    uint64     `json:"leader_commit"`
}

// AppendEntriesResponse answers an AppendEntriesRequest. On a log mismatch it
// carries conflict hints so the leader can back up efficiently.
type AppendEntriesResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	SourceNodeID    string `json:"source_node_id"`
	Term            uint64 `json:"term"`
	Success         bool   `json:"success"`
	MatchIndex      uint64 `json:"match_index"`
	ConflictTerm    uint64 `json:"conflict_term"`
	ConflictIndex   uint64 `json:"conflict_index"`
}

// PeerProgress reports a leader's replication progress for one follower.
type PeerProgress struct {
	NodeID     string `json:"node_id"`
	NextIndex  uint64 `json:"next_index"`
	MatchIndex uint64 `json:"match_index"`
}

// Status is an immutable snapshot of a node's consensus state.
type Status struct {
	NodeID              string         `json:"node_id"`
	Role                Role           `json:"role"`
	Term                uint64         `json:"term"`
	LeaderID            string         `json:"leader_id"`
	LastLogIndex        uint64         `json:"last_log_index"`
	LastLogTerm         uint64         `json:"last_log_term"`
	CommitIndex         uint64         `json:"commit_index"`
	LastApplied         uint64         `json:"last_applied"`
	LastLeaderContactMS int64          `json:"last_leader_contact_ms,omitempty"`
	LastQuorumContactMS int64          `json:"last_quorum_contact_ms,omitempty"`
	Peers               []PeerProgress `json:"peers,omitempty"`
}
