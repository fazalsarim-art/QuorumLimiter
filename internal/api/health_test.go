package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

type fakeHealthNode struct {
	status raft.Status
	err    error
}

func (f *fakeHealthNode) Status() (raft.Status, error) { return f.status, f.err }

func readyReq(t *testing.T, h *HealthHandler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleReady(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestHealthLive(t *testing.T) {
	h := NewHealthHandler(&fakeHealthNode{}, "node1", 100*time.Millisecond)
	rec := httptest.NewRecorder()
	h.handleLive(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "alive" || body["node_id"] != "node1" {
		t.Errorf("body = %v", body)
	}
}

func TestReadinessStates(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	fresh := now.Add(-50 * time.Millisecond).UnixMilli() // within 5*100ms window
	stale := now.Add(-2 * time.Second).UnixMilli()

	cases := []struct {
		name      string
		status    raft.Status
		draining  bool
		wantCode  int
		wantState string
	}{
		{"leader with quorum", raft.Status{Role: raft.RoleLeader, LastQuorumContactMS: fresh}, false, 200, "ready"},
		{"leader no quorum", raft.Status{Role: raft.RoleLeader, LastQuorumContactMS: stale}, false, 503, "no_quorum"},
		{"leader never contacted", raft.Status{Role: raft.RoleLeader}, false, 503, "no_quorum"},
		{"candidate", raft.Status{Role: raft.RoleCandidate}, false, 503, "no_leader"},
		{"follower ready", raft.Status{Role: raft.RoleFollower, LeaderID: "node2", LastLeaderContactMS: fresh}, false, 200, "ready"},
		{"follower no leader", raft.Status{Role: raft.RoleFollower}, false, 503, "no_leader"},
		{"follower stale leader", raft.Status{Role: raft.RoleFollower, LeaderID: "node2", LastLeaderContactMS: stale}, false, 503, "no_leader"},
		{"follower lagging", raft.Status{Role: raft.RoleFollower, LeaderID: "node2", LastLeaderContactMS: fresh, CommitIndex: 500, LastApplied: 100}, false, 503, "lagging"},
		{"draining", raft.Status{Role: raft.RoleLeader, LastQuorumContactMS: fresh}, true, 503, "shutting_down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHealthHandler(&fakeHealthNode{status: tc.status}, "node1", 100*time.Millisecond)
			h.now = func() time.Time { return now }
			h.SetDraining(tc.draining)
			code, body := readyReq(t, h)
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if body["status"] != tc.wantState {
				t.Errorf("state = %v, want %s", body["status"], tc.wantState)
			}
		})
	}
}
