// Package config parses, validates, and holds the runtime configuration for a
// single QuorumLimiter node. Every value is read from the environment and fully
// validated before the process begins any concurrency or network activity, so a
// misconfigured node fails fast with a readable list of problems instead of
// misbehaving later.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// wantPeers is the fixed cluster size for this educational build.
	wantPeers = 3
	// minTokenLen is the minimum length for the string bearer/admin secrets.
	minTokenLen = 16
	// minKeyBytes is the minimum decoded length for the HMAC/signing secrets.
	minKeyBytes = 32
	// heartbeatsPerElection is the minimum ratio between the shortest election
	// timeout and the heartbeat interval, so heartbeats reliably beat elections.
	heartbeatsPerElection = 3
)

// Peer is one member of the fixed cluster, identified by node ID and reachable
// at a base URL used for internal Raft RPCs.
type Peer struct {
	ID  string
	URL string
}

// Config holds all validated configuration for one node. Secret fields must
// never be logged; see LogValue, which deliberately omits them.
type Config struct {
	NodeID       string
	ClusterID    string
	BindAddr     string
	AdvertiseURL string
	Peers        []Peer
	DataPath     string

	ElectionTimeoutMin time.Duration
	ElectionTimeoutMax time.Duration
	HeartbeatInterval  time.Duration
	ProposalTimeout    time.Duration

	PublicBaseURL  string
	Production     bool
	LogLevel       slog.Level
	RevealSubjects bool

	// Secrets. Never log these.
	ClusterToken string
	AdminToken   string
	SessionKey   []byte
	APIKeyPepper []byte
}

// LookupFunc reports the value of an environment variable and whether it was
// set, matching the signature of os.LookupEnv. It is injected so tests can
// supply configuration without mutating global process state.
type LookupFunc func(key string) (string, bool)

// Load reads and validates configuration from the OS environment.
func Load(lookup LookupFunc) (*Config, error) {
	var errs []error
	addErr := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	get := func(key string) string {
		v, _ := lookup(key)
		return strings.TrimSpace(v)
	}
	requireStr := func(key string, minLen int) string {
		v := get(key)
		switch {
		case v == "":
			addErr(fmt.Errorf("%s is required", key))
		case len(v) < minLen:
			addErr(fmt.Errorf("%s must be at least %d characters", key, minLen))
		}
		return v
	}
	requireDur := func(key string) time.Duration {
		v := get(key)
		if v == "" {
			addErr(fmt.Errorf("%s is required", key))
			return 0
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			addErr(fmt.Errorf("%s is not a valid duration (e.g. 800ms, 3s)", key))
			return 0
		}
		if d <= 0 {
			addErr(fmt.Errorf("%s must be positive", key))
		}
		return d
	}
	requireBool := func(key string) bool {
		v := get(key)
		if v == "" {
			addErr(fmt.Errorf("%s is required", key))
			return false
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			addErr(fmt.Errorf("%s must be true or false", key))
		}
		return b
	}
	// decodeSecret base64-decodes a secret without ever placing its value in an
	// error message.
	decodeSecret := func(key string) []byte {
		v := get(key)
		if v == "" {
			addErr(fmt.Errorf("%s is required", key))
			return nil
		}
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			addErr(fmt.Errorf("%s must be valid base64", key))
			return nil
		}
		if len(raw) < minKeyBytes {
			addErr(fmt.Errorf("%s must decode to at least %d bytes", key, minKeyBytes))
		}
		return raw
	}

	cfg := &Config{
		NodeID:       get("QL_NODE_ID"),
		ClusterID:    get("QL_CLUSTER_ID"),
		BindAddr:     get("QL_BIND_ADDR"),
		AdvertiseURL: get("QL_ADVERTISE_URL"),
		DataPath:     get("QL_DATA_PATH"),

		ElectionTimeoutMin: requireDur("QL_ELECTION_TIMEOUT_MIN"),
		ElectionTimeoutMax: requireDur("QL_ELECTION_TIMEOUT_MAX"),
		HeartbeatInterval:  requireDur("QL_HEARTBEAT_INTERVAL"),
		ProposalTimeout:    requireDur("QL_PROPOSAL_TIMEOUT"),

		PublicBaseURL: get("QL_PUBLIC_BASE_URL"),
		Production:    requireBool("QL_PRODUCTION"),

		ClusterToken: requireStr("QL_CLUSTER_TOKEN", minTokenLen),
		AdminToken:   requireStr("QL_ADMIN_TOKEN", minTokenLen),
		SessionKey:   decodeSecret("QL_SESSION_KEY"),
		APIKeyPepper: decodeSecret("QL_API_KEY_PEPPER"),
	}

	// Required non-secret string fields.
	if cfg.NodeID == "" {
		addErr(errors.New("QL_NODE_ID is required"))
	}
	if cfg.ClusterID == "" {
		addErr(errors.New("QL_CLUSTER_ID is required"))
	}
	if cfg.BindAddr == "" {
		addErr(errors.New("QL_BIND_ADDR is required"))
	}
	if cfg.DataPath == "" {
		addErr(errors.New("QL_DATA_PATH is required"))
	}
	addErr(validateURL("QL_ADVERTISE_URL", cfg.AdvertiseURL))
	addErr(validateURL("QL_PUBLIC_BASE_URL", cfg.PublicBaseURL))

	// Peers.
	peers, err := parsePeers(get("QL_PEERS"))
	if err != nil {
		addErr(err)
	} else {
		cfg.Peers = peers
		if len(peers) != wantPeers {
			addErr(fmt.Errorf("QL_PEERS must list exactly %d nodes, got %d", wantPeers, len(peers)))
		}
		if cfg.NodeID != "" && !containsPeer(peers, cfg.NodeID) {
			addErr(fmt.Errorf("QL_NODE_ID %q must be one of the peers in QL_PEERS", cfg.NodeID))
		}
	}

	// Timeout relationships (only meaningful once each parsed cleanly).
	if cfg.HeartbeatInterval > 0 && cfg.ElectionTimeoutMin > 0 {
		if cfg.ElectionTimeoutMin <= heartbeatsPerElection*cfg.HeartbeatInterval {
			addErr(fmt.Errorf(
				"QL_ELECTION_TIMEOUT_MIN (%s) must be greater than %d heartbeat intervals (%s)",
				cfg.ElectionTimeoutMin, heartbeatsPerElection,
				heartbeatsPerElection*cfg.HeartbeatInterval))
		}
	}
	if cfg.ElectionTimeoutMin > 0 && cfg.ElectionTimeoutMax > 0 {
		if cfg.ElectionTimeoutMax <= cfg.ElectionTimeoutMin {
			addErr(fmt.Errorf(
				"QL_ELECTION_TIMEOUT_MAX (%s) must be greater than QL_ELECTION_TIMEOUT_MIN (%s)",
				cfg.ElectionTimeoutMax, cfg.ElectionTimeoutMin))
		}
	}

	// Log level (optional, defaults to info).
	level, err := parseLevel(get("QL_LOG_LEVEL"))
	if err != nil {
		addErr(err)
	}
	cfg.LogLevel = level

	// Reveal subjects (optional, defaults to false).
	if v := get("QL_REVEAL_SUBJECTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			addErr(errors.New("QL_REVEAL_SUBJECTS must be true or false"))
		}
		cfg.RevealSubjects = b
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// PeerURL returns the advertised URL for the given node ID, if present.
func (c *Config) PeerURL(nodeID string) (string, bool) {
	for _, p := range c.Peers {
		if p.ID == nodeID {
			return p.URL, true
		}
	}
	return "", false
}

// LogValue implements slog.LogValuer so that logging a Config never exposes any
// secret. Secret fields are intentionally omitted.
func (c *Config) LogValue() slog.Value {
	peerIDs := make([]string, len(c.Peers))
	for i, p := range c.Peers {
		peerIDs[i] = p.ID
	}
	return slog.GroupValue(
		slog.String("node_id", c.NodeID),
		slog.String("cluster_id", c.ClusterID),
		slog.String("bind_addr", c.BindAddr),
		slog.String("advertise_url", c.AdvertiseURL),
		slog.Any("peers", peerIDs),
		slog.String("data_path", c.DataPath),
		slog.Duration("election_timeout_min", c.ElectionTimeoutMin),
		slog.Duration("election_timeout_max", c.ElectionTimeoutMax),
		slog.Duration("heartbeat_interval", c.HeartbeatInterval),
		slog.Duration("proposal_timeout", c.ProposalTimeout),
		slog.String("public_base_url", c.PublicBaseURL),
		slog.Bool("production", c.Production),
		slog.String("log_level", c.LogLevel.String()),
		slog.Bool("reveal_subjects", c.RevealSubjects),
	)
}

func validateURL(key, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", key)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL like http://host:port", key)
	}
	return nil
}

func parsePeers(raw string) ([]Peer, error) {
	if raw == "" {
		return nil, errors.New("QL_PEERS is required")
	}
	var peers []Peer
	seenID := make(map[string]bool)
	seenURL := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, u, ok := strings.Cut(part, "=")
		id = strings.TrimSpace(id)
		u = strings.TrimSpace(u)
		if !ok || id == "" || u == "" {
			return nil, fmt.Errorf("QL_PEERS entry %q must be nodeID=URL", part)
		}
		if seenID[id] {
			return nil, fmt.Errorf("QL_PEERS has duplicate node ID %q", id)
		}
		if seenURL[u] {
			return nil, fmt.Errorf("QL_PEERS has duplicate URL %q", u)
		}
		if parsed, err := url.Parse(u); err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("QL_PEERS node %q has invalid URL %q", id, u)
		}
		seenID[id] = true
		seenURL[u] = true
		peers = append(peers, Peer{ID: id, URL: u})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers, nil
}

func containsPeer(peers []Peer, id string) bool {
	for _, p := range peers {
		if p.ID == id {
			return true
		}
	}
	return false
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("QL_LOG_LEVEL %q must be one of debug, info, warn, error", s)
	}
}
