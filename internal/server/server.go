// Package server wraps a provided HTTP handler with bounded timeouts and a
// graceful lifecycle. Route registration (health, public, admin, internal,
// metrics) is done by the caller, which passes the finished handler here.
package server

import (
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

// Server owns the node's *http.Server and its lifecycle.
type Server struct {
	http *http.Server
	log  *slog.Logger
}

// New builds a Server that serves handler at the configured bind address with
// bounded timeouts.
func New(cfg *config.Config, log *slog.Logger, handler http.Handler) *Server {
	return &Server{
		log: log,
		http: &http.Server{
			Addr:              cfg.BindAddr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		},
	}
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
