// Command quorumlimiter is the single executable that runs one node of the
// distributed rate limiter. In Phase 1 it loads and validates configuration,
// sets up structured logging, serves a liveness endpoint, and shuts down
// gracefully. Consensus, storage, and application behavior arrive in later
// phases.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/fazalsarim-art/QuorumLimiter/internal/config"
	"github.com/fazalsarim-art/QuorumLimiter/internal/server"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		// Fail fast with a readable, one-per-line list of problems.
		fmt.Fprintln(os.Stderr, "configuration error:")
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(os.Stderr, "  - "+line)
		}
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	})).With(
		slog.String("node_id", cfg.NodeID),
		slog.String("version", version),
	)
	slog.SetDefault(logger)

	logger.Info("starting quorumlimiter", slog.Any("config", cfg))

	srv := server.New(cfg, logger)

	// Run the server; a fatal listen error is reported on errCh.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("bind_addr", srv.Addr()))
		errCh <- srv.Start()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("http server failed", slog.String("error", err.Error()))
			return 1
		}
		return 0
	case <-ctx.Done():
		stop() // restore default signal handling; a second signal now aborts
		logger.Info("shutdown signal received, stopping")
		if err := srv.Shutdown(context.Background()); err != nil {
			logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
			return 1
		}
		logger.Info("shutdown complete")
		return 0
	}
}
