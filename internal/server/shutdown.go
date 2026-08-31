package server

import (
	"context"
	"time"
)

// ShutdownTimeout bounds how long a graceful shutdown may take before the
// server stops waiting for in-flight requests to drain.
const ShutdownTimeout = 5 * time.Second

// Shutdown gracefully stops the HTTP server, waiting up to ShutdownTimeout for
// in-flight requests to complete. The provided context can cancel the wait
// sooner.
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
	defer cancel()
	return s.http.Shutdown(ctx)
}
