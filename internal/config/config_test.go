package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
)

// validEnv returns a complete, valid environment map for node1.
func validEnv() map[string]string {
	secret32 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 48))
	return map[string]string{
		"QL_NODE_ID":              "node1",
		"QL_CLUSTER_ID":           "quorumlimiter-local",
		"QL_BIND_ADDR":            "127.0.0.1:18081",
		"QL_ADVERTISE_URL":        "http://127.0.0.1:18081",
		"QL_PEERS":                "node1=http://127.0.0.1:18081,node2=http://127.0.0.1:18082,node3=http://127.0.0.1:18083",
		"QL_DATA_PATH":            "./data/node1/quorumlimiter.db",
		"QL_CLUSTER_TOKEN":        "cluster-token-abcdefghij",
		"QL_ADMIN_TOKEN":          "admin-token-abcdefghij",
		"QL_SESSION_KEY":          secret32,
		"QL_API_KEY_PEPPER":       secret32,
		"QL_ELECTION_TIMEOUT_MIN": "800ms",
		"QL_ELECTION_TIMEOUT_MAX": "1400ms",
		"QL_HEARTBEAT_INTERVAL":   "200ms",
		"QL_PROPOSAL_TIMEOUT":     "3s",
		"QL_PUBLIC_BASE_URL":      "http://localhost:8080",
		"QL_PRODUCTION":           "false",
		"QL_LOG_LEVEL":            "info",
	}
}

// lookupFrom turns a map into a LookupFunc.
func lookupFrom(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.NodeID != "node1" {
		t.Errorf("NodeID = %q, want node1", cfg.NodeID)
	}
	if len(cfg.Peers) != 3 {
		t.Fatalf("len(Peers) = %d, want 3", len(cfg.Peers))
	}
	// Peers should be sorted by ID.
	if cfg.Peers[0].ID != "node1" || cfg.Peers[2].ID != "node3" {
		t.Errorf("peers not sorted: %+v", cfg.Peers)
	}
	if cfg.HeartbeatInterval.String() != "200ms" {
		t.Errorf("HeartbeatInterval = %s, want 200ms", cfg.HeartbeatInterval)
	}
	if len(cfg.SessionKey) < minKeyBytes {
		t.Errorf("SessionKey decoded len = %d, want >= %d", len(cfg.SessionKey), minKeyBytes)
	}
	if len(cfg.APIKeyPepper) < minKeyBytes {
		t.Errorf("APIKeyPepper decoded len = %d, want >= %d", len(cfg.APIKeyPepper), minKeyBytes)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %s, want INFO", cfg.LogLevel)
	}
	if url, ok := cfg.PeerURL("node2"); !ok || url != "http://127.0.0.1:18082" {
		t.Errorf("PeerURL(node2) = %q,%v", url, ok)
	}
}

func TestLoadDefaults(t *testing.T) {
	env := validEnv()
	delete(env, "QL_LOG_LEVEL") // optional
	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("default LogLevel = %s, want INFO", cfg.LogLevel)
	}
	if cfg.RevealSubjects {
		t.Errorf("default RevealSubjects = true, want false")
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(env map[string]string)
		wantSub string // substring expected in the error
	}{
		{"missing node id", func(e map[string]string) { delete(e, "QL_NODE_ID") }, "QL_NODE_ID"},
		{"missing cluster token", func(e map[string]string) { delete(e, "QL_CLUSTER_TOKEN") }, "QL_CLUSTER_TOKEN"},
		{"short admin token", func(e map[string]string) { e["QL_ADMIN_TOKEN"] = "short" }, "QL_ADMIN_TOKEN"},
		{"duplicate peer id", func(e map[string]string) {
			e["QL_PEERS"] = "node1=http://a:1,node1=http://b:2,node3=http://c:3"
		}, "duplicate node ID"},
		{"duplicate peer url", func(e map[string]string) {
			e["QL_PEERS"] = "node1=http://a:1,node2=http://a:1,node3=http://c:3"
		}, "duplicate URL"},
		{"wrong peer count", func(e map[string]string) {
			e["QL_PEERS"] = "node1=http://127.0.0.1:18081,node2=http://127.0.0.1:18082"
		}, "exactly 3 nodes"},
		{"local node not a peer", func(e map[string]string) { e["QL_NODE_ID"] = "node9" }, "must be one of the peers"},
		{"election min too small", func(e map[string]string) {
			e["QL_HEARTBEAT_INTERVAL"] = "200ms"
			e["QL_ELECTION_TIMEOUT_MIN"] = "600ms" // == 3 * heartbeat, must be strictly greater
		}, "QL_ELECTION_TIMEOUT_MIN"},
		{"election max not greater than min", func(e map[string]string) {
			e["QL_ELECTION_TIMEOUT_MIN"] = "1000ms"
			e["QL_ELECTION_TIMEOUT_MAX"] = "1000ms"
		}, "QL_ELECTION_TIMEOUT_MAX"},
		{"invalid duration", func(e map[string]string) { e["QL_HEARTBEAT_INTERVAL"] = "fast" }, "QL_HEARTBEAT_INTERVAL"},
		{"bad base64 session key", func(e map[string]string) { e["QL_SESSION_KEY"] = "not base64!!!" }, "QL_SESSION_KEY"},
		{"short session key", func(e map[string]string) {
			e["QL_SESSION_KEY"] = base64.StdEncoding.EncodeToString([]byte("tooshort"))
		}, "at least 32 bytes"},
		{"invalid production bool", func(e map[string]string) { e["QL_PRODUCTION"] = "maybe" }, "QL_PRODUCTION"},
		{"invalid log level", func(e map[string]string) { e["QL_LOG_LEVEL"] = "chatty" }, "QL_LOG_LEVEL"},
		{"invalid advertise url", func(e map[string]string) { e["QL_ADVERTISE_URL"] = "not-a-url" }, "QL_ADVERTISE_URL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			tc.mutate(env)
			_, err := Load(lookupFrom(env))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestLogValueRedactsSecrets ensures logging a Config never leaks any secret.
func TestLogValueRedactsSecrets(t *testing.T) {
	env := validEnv()
	env["QL_CLUSTER_TOKEN"] = "SUPER-SECRET-CLUSTER-TOKEN"
	env["QL_ADMIN_TOKEN"] = "SUPER-SECRET-ADMIN-TOKEN"
	cfg, err := Load(lookupFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config loaded", slog.Any("config", cfg))

	out := buf.String()
	for _, secret := range []string{
		"SUPER-SECRET-CLUSTER-TOKEN",
		"SUPER-SECRET-ADMIN-TOKEN",
		base64.StdEncoding.EncodeToString(cfg.SessionKey),
		base64.StdEncoding.EncodeToString(cfg.APIKeyPepper),
	} {
		if strings.Contains(out, secret) {
			t.Errorf("log output leaked a secret: %s", out)
		}
	}
}
