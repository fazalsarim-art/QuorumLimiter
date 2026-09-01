package api

import "net/http"

// This file centralizes HTTP route registration for the api package. Route
// groups are added as their phases land: internal Raft RPCs (Phase 5), the
// public decision API (Phase 7), and admin endpoints (Phase 8).

// RegisterInternalRaft registers the private Raft RPC endpoints on mux. These
// must never be exposed through the public gateway.
func RegisterInternalRaft(mux *http.ServeMux, h *InternalRaftHandler) {
	mux.HandleFunc("POST /internal/raft/request-vote", h.handleRequestVote)
	mux.HandleFunc("POST /internal/raft/append-entries", h.handleAppendEntries)
	mux.HandleFunc("GET /internal/raft/status", h.handleStatus)
}
