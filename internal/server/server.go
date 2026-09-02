// Package server wires up the node's HTTP server: timeouts, route registration,
// and lifecycle. In Phase 1 it serves only the liveness endpoint; later phases
// register the public, admin, health, metrics, and internal route groups.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/config"
)

// Timeouts applied to the HTTP server. These bound slow or stalled clients so a
// connection cannot be held open indefinitely.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

// Server owns the node's *http.Server and its dependencies.
type Server struct {
	http   *http.Server
	log    *slog.Logger
	nodeID string
}

// New builds a Server with the configured bind address, timeouts, and routes.
// Additional route groups (public, admin, internal) are attached via registrars,
// which run after the built-in health route.
func New(cfg *config.Config, log *slog.Logger, registrars ...func(*http.ServeMux)) *Server {
	s := &Server{log: log, nodeID: cfg.NodeID}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.handleLive)
	for _, register := range registrars {
		register(mux)
	}

	s.http = &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	return s
}

// Addr returns the address the server is configured to listen on.
func (s *Server) Addr() string { return s.http.Addr }

// Start begins serving and blocks until the server is closed. A clean shutdown
// via Shutdown returns nil rather than http.ErrServerClosed.
func (s *Server) Start() error {
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handleLive reports process liveness. It never consults consensus state, so it
// returns 200 whenever the HTTP process is running.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "alive",
		"node_id": s.nodeID,
	})
}
