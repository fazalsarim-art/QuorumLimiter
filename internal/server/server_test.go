package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/config"
)

// TestStartShutdown starts a real listener serving a provided handler, confirms
// it responds, then verifies graceful shutdown returns promptly and Start
// returns nil.
func TestStartShutdown(t *testing.T) {
	addr := freeAddr(t)
	cfg := &config.Config{NodeID: "node1", BindAddr: addr}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s := New(cfg, slog.Default(), mux)

	started := make(chan error, 1)
	go func() { started <- s.Start() }()

	url := "http://" + addr + "/ping"
	waitReady(t, url)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)

	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown returned error: %v", err)
		}
	case <-time.After(ShutdownTimeout + time.Second):
		t.Fatal("Shutdown did not complete in time")
	}

	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start returned error after clean shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after shutdown")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become ready in time")
}
