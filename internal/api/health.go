package api

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

// HealthNode is the consensus surface the health endpoints read.
type HealthNode interface {
	Status() (raft.Status, error)
}

// lagThreshold is how far a follower's applied index may trail commit before it
// is considered "lagging" and not ready.
const lagThreshold uint64 = 64

// HealthHandler serves /health/live and /health/ready.
type HealthHandler struct {
	node        HealthNode
	nodeID      string
	staleWindow time.Duration
	draining    atomic.Bool
	now         func() time.Time
}

// NewHealthHandler builds a health handler. staleWindow (typically a few
// heartbeat intervals) bounds how old the last quorum/leader contact may be.
func NewHealthHandler(node HealthNode, nodeID string, heartbeat time.Duration) *HealthHandler {
	if heartbeat <= 0 {
		heartbeat = 200 * time.Millisecond
	}
	return &HealthHandler{
		node:        node,
		nodeID:      nodeID,
		staleWindow: 5 * heartbeat,
		now:         time.Now,
	}
}

// SetDraining marks the node as shutting down so readiness fails fast.
func (h *HealthHandler) SetDraining(v bool) { h.draining.Store(v) }

func (h *HealthHandler) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive", "node_id": h.nodeID})
}

func (h *HealthHandler) handleReady(w http.ResponseWriter, _ *http.Request) {
	s, err := h.node.Status()
	state, ready := h.readiness(s, err)
	code := http.StatusServiceUnavailable
	if ready {
		code = http.StatusOK
	}
	body := map[string]any{"status": state, "node_id": h.nodeID}
	if err == nil {
		body["role"] = s.Role.String()
		body["term"] = s.Term
		body["leader_id"] = s.LeaderID
		body["commit_index"] = s.CommitIndex
		body["last_applied"] = s.LastApplied
	}
	writeJSON(w, code, body)
}

// readiness classifies the node's ability to serve. States mirror the documented
// set: shutting_down, starting, no_leader, no_quorum, lagging, ready.
func (h *HealthHandler) readiness(s raft.Status, err error) (string, bool) {
	if h.draining.Load() {
		return "shutting_down", false
	}
	if err != nil {
		return "starting", false
	}
	now := h.now().UnixMilli()
	stale := h.staleWindow.Milliseconds()
	switch s.Role {
	case raft.RoleLeader:
		if s.LastQuorumContactMS == 0 || now-s.LastQuorumContactMS > stale {
			return "no_quorum", false
		}
		return "ready", true
	case raft.RoleCandidate:
		return "no_leader", false
	default: // follower
		if s.LeaderID == "" || s.LastLeaderContactMS == 0 || now-s.LastLeaderContactMS > stale {
			return "no_leader", false
		}
		if s.CommitIndex > s.LastApplied && s.CommitIndex-s.LastApplied > lagThreshold {
			return "lagging", false
		}
		return "ready", true
	}
}
