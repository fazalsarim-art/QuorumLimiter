package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDecideAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer testkey" {
			t.Errorf("missing auth header")
		}
		if r.Header.Get("Idempotency-Key") != "idem-000000000001" {
			t.Errorf("missing idempotency header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"allowed":true,"remaining_milli":4000,"log_index":9}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "testkey")
	resp, err := c.Decide(context.Background(), DecideRequest{PolicyID: "pol", Subject: "s", Cost: 1}, "idem-000000000001")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !resp.Allowed || resp.RemainingMilli != 4000 || resp.StatusCode != 200 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestDecideDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"allowed":false,"retry_after_ms":5000}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	resp, err := c.Decide(context.Background(), DecideRequest{PolicyID: "pol", Subject: "s", Cost: 1}, "idem-000000000001")
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if resp.Allowed || resp.StatusCode != 429 || resp.RetryAfterMS != 5000 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestDecideAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"bad key"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	_, err := c.Decide(context.Background(), DecideRequest{PolicyID: "pol", Subject: "s", Cost: 1}, "idem-000000000001")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != 401 || apiErr.Code != "invalid_api_key" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestRetriesOnlyWithIdempotencyKey(t *testing.T) {
	// Server returns 503 twice then 200; count attempts.
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"leader_unavailable"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer srv.Close()

	// With an idempotency key: retries until success (maxRetries=2 -> 3 attempts).
	c := New(srv.URL, "k", WithMaxRetries(2))
	resp, err := c.Decide(context.Background(), DecideRequest{PolicyID: "pol", Subject: "s", Cost: 1}, "idem-000000000001")
	if err != nil {
		t.Fatalf("Decide with key: %v", err)
	}
	if !resp.Allowed {
		t.Errorf("expected allowed after retries")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}

	// Without an idempotency key: no retry — a single 503 surfaces as an error.
	attempts.Store(0)
	_, err = c.Decide(context.Background(), DecideRequest{PolicyID: "pol", Subject: "s", Cost: 1}, "")
	if err == nil {
		t.Fatal("expected error without idempotency key")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts without key = %d, want 1 (no retry)", got)
	}
}
