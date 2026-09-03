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

// RegisterDecision registers the public decision endpoint on mux.
func RegisterDecision(mux *http.ServeMux, h *DecisionHandler) {
	mux.HandleFunc("POST /v1/decisions", h.handleDecision)
}

// RegisterHealth registers the liveness and readiness endpoints on mux.
func RegisterHealth(mux *http.ServeMux, h *HealthHandler) {
	mux.HandleFunc("GET /health/live", h.handleLive)
	mux.HandleFunc("GET /health/ready", h.handleReady)
}

// RegisterMetrics registers the Prometheus metrics endpoint on mux.
func RegisterMetrics(mux *http.ServeMux, handler http.Handler) {
	mux.Handle("GET /metrics", handler)
}

// RegisterAdmin registers the Raft-backed admin JSON APIs under /api/admin.
// GET APIs require a session; mutating APIs also require same-origin + CSRF.
// Session sign-in lives in the dashboard package.
func RegisterAdmin(mux *http.ServeMux, h *AdminHandler) {
	mux.HandleFunc("GET /api/admin/policies", h.requireAdmin(false, h.listPolicies))
	mux.HandleFunc("POST /api/admin/policies", h.requireAdmin(true, h.createPolicy))
	mux.HandleFunc("PUT /api/admin/policies/{id}", h.requireAdmin(true, h.updatePolicy))
	mux.HandleFunc("POST /api/admin/policies/{id}/activation", h.requireAdmin(true, h.changeActivation))

	mux.HandleFunc("GET /api/admin/clients", h.requireAdmin(false, h.listClients))
	mux.HandleFunc("POST /api/admin/clients", h.requireAdmin(true, h.createClient))
	mux.HandleFunc("POST /api/admin/clients/{id}/revoke", h.requireAdmin(true, h.revokeClient))
}
