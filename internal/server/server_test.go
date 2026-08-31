package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/config"
)

func TestHandleLive(t *testing.T) {
	s := &Server{nodeID: "node1", log: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	s.handleLive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["status"] != "alive" {
		t.Errorf("status = %q, want alive", body["status"])
	}
	if body["node_id"] != "node1" {
		t.Errorf("node_id = %q, want node1", body["node_id"])
	}
}

// TestStartShutdown starts a real listener, confirms /health/live responds, then
// verifies graceful shutdown returns promptly and Start returns nil.
func TestStartShutdown(t *testing.T) {
	addr := freeAddr(t)
	cfg := &config.Config{NodeID: "node1", BindAddr: addr}
	s := New(cfg, slog.Default())

	started := make(chan error, 1)
	go func() { started <- s.Start() }()

	// Poll until the server accepts connections.
	url := "http://" + addr + "/health/live"
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

	// Graceful shutdown must complete well within the timeout.
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

// freeAddr returns a currently-free loopback address.
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
