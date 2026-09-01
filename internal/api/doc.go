// Package api defines the HTTP routes, request/response data transfer objects,
// validation, middleware, and the internal Raft RPC handlers, plus the HTTP
// transport that implements raft.Transport.
//
// Phase 5 adds the private /internal/raft/* endpoints and the HTTP transport.
// The public decision API is added in Phase 7 and admin endpoints in Phase 8.
package api
