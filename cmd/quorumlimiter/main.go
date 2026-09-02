// Command quorumlimiter is the single executable that runs one node of the
// distributed rate limiter. It loads and validates configuration, opens durable
// storage, starts the Raft consensus node with an HTTP transport, serves the
// public decision API (forwarding writes to the leader) and the private Raft
// endpoints, and shuts down gracefully.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/api"
	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/config"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
	"github.com/fazalsarim-art/QuorumLimiter/internal/server"
	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
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

	store, err := storage.Open(cfg.DataPath)
	if err != nil {
		logger.Error("failed to open storage", slog.String("error", err.Error()))
		return 1
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			logger.Error("failed to close storage", slog.String("error", cerr.Error()))
		}
	}()
	logger.Info("storage opened", slog.String("data_path", store.Path()))

	// Peer addressing.
	peerURLs := make(map[string]string, len(cfg.Peers))
	peerIDs := make([]string, 0, len(cfg.Peers))
	otherPeerIDs := make([]string, 0, len(cfg.Peers)-1)
	for _, p := range cfg.Peers {
		peerURLs[p.ID] = p.URL
		peerIDs = append(peerIDs, p.ID)
		if p.ID != cfg.NodeID {
			otherPeerIDs = append(otherPeerIDs, p.ID)
		}
	}

	// Consensus: state machine, HTTP transport, and the Raft node.
	sm := limiter.New()
	transport := api.NewHTTPTransport(peerURLs, cfg.ClusterToken, &http.Client{Timeout: 2 * time.Second})
	apply := func(index, term uint64, entry raft.LogEntry) (any, error) {
		return sm.ApplyEntry(store, index, term, entry)
	}
	node, err := raft.New(raft.Config{
		NodeID:             cfg.NodeID,
		ClusterID:          cfg.ClusterID,
		Peers:              peerIDs,
		ElectionTimeoutMin: cfg.ElectionTimeoutMin,
		ElectionTimeoutMax: cfg.ElectionTimeoutMax,
		HeartbeatInterval:  cfg.HeartbeatInterval,
	}, raft.Deps{Store: store, Transport: transport, Apply: apply, Logger: logger})
	if err != nil {
		logger.Error("failed to start raft node", slog.String("error", err.Error()))
		return 1
	}
	// Stop the node before the store closes (deferred LIFO).
	defer node.Stop()
	logger.Info("raft node started")

	// HTTP handlers.
	authn := auth.NewAPIKeyAuthenticator(cfg.APIKeyPepper, store)
	internalRaft := api.NewInternalRaftHandler(node, cfg.ClusterToken, otherPeerIDs, logger)
	decisions := api.NewDecisionHandler(node, authn, cfg.NodeID, peerURLs, cfg.ProposalTimeout, logger)

	srv := server.New(cfg, logger,
		func(mux *http.ServeMux) { api.RegisterInternalRaft(mux, internalRaft) },
		func(mux *http.ServeMux) { api.RegisterDecision(mux, decisions) },
	)

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
