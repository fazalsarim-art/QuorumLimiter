package dashboard

import (
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

type loginData struct {
	Error bool
}

type overviewData struct {
	LeaderID      string
	Term          uint64
	Role          string
	CommitIndex   uint64
	LastApplied   uint64
	PolicyCount   int
	ClientCount   int
	RecentTotal   int
	AllowedRecent int
	DeniedRecent  int
	Policies      []limiter.Policy // active policies for the quick-test dropdown
}

type policiesData struct {
	Policies []limiter.Policy
	Status   string
	Query    string
}

type clientsData struct {
	Clients     []limiter.Client
	NewKey      string
	NewClientID string
}

type decisionsData struct {
	Audits []limiter.AuditEvent
}

type clusterData struct {
	Status raft.Status
}

type docsData struct {
	BaseURL string
}

// decodePolicies decodes and returns policies, optionally filtered.
func decodePolicies(raw [][]byte) []limiter.Policy {
	out := make([]limiter.Policy, 0, len(raw))
	for _, b := range raw {
		if p, err := limiter.DecodePolicy(b); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func decodeClients(raw [][]byte) []limiter.Client {
	out := make([]limiter.Client, 0, len(raw))
	for _, b := range raw {
		if c, err := limiter.DecodeClient(b); err == nil {
			out = append(out, c)
		}
	}
	return out
}
