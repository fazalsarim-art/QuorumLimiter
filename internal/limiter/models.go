package limiter

import (
	"encoding/json"
	"fmt"
)

// ModelSchemaVersion is the version stamped into every stored application value.
const ModelSchemaVersion uint32 = 1

// Policy is a token-bucket configuration. All token quantities are whole tokens;
// bucket balances are tracked separately in milli-tokens.
type Policy struct {
	SchemaVersion    uint32 `json:"schema_version"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	CapacityTokens   int64  `json:"capacity_tokens"`
	RefillTokens     int64  `json:"refill_tokens"`
	RefillIntervalMS int64  `json:"refill_interval_ms"`
	MaxCostTokens    int64  `json:"max_cost_tokens"`
	Active           bool   `json:"active"`
	Version          uint64 `json:"version"`
	CreatedAtMS      int64  `json:"created_at_ms"`
	UpdatedAtMS      int64  `json:"updated_at_ms"`
}

// Client is an API client. The raw key is never stored; only its prefix (for
// lookup) and an HMAC digest (for constant-time verification) are kept.
type Client struct {
	SchemaVersion    uint32   `json:"schema_version"`
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	KeyPrefix        string   `json:"key_prefix"`
	KeyDigest        []byte   `json:"key_digest"`
	AllowedPolicyIDs []string `json:"allowed_policy_ids"`
	Active           bool     `json:"active"`
	CreatedAtMS      int64    `json:"created_at_ms"`
	RevokedAtMS      *int64   `json:"revoked_at_ms"`
}

// AllowsPolicy reports whether the client may use the given policy.
func (c Client) AllowsPolicy(policyID string) bool {
	for _, p := range c.AllowedPolicyIDs {
		if p == "*" || p == policyID {
			return true
		}
	}
	return false
}

// TokenBucketState is the per-subject balance for a policy. tokens_milli holds
// milli-tokens (1000 = one token); refill_remainder preserves the integer
// division remainder so no fractional credit is lost across refills.
type TokenBucketState struct {
	SchemaVersion   uint32 `json:"schema_version"`
	PolicyID        string `json:"policy_id"`
	Subject         string `json:"subject"`
	TokensMilli     int64  `json:"tokens_milli"`
	LastRefillMS    int64  `json:"last_refill_ms"`
	RefillRemainder int64  `json:"refill_remainder"`
	PolicyVersion   uint64 `json:"policy_version"`
	UpdatedLogIndex uint64 `json:"updated_log_index"`
}

// DecisionRecord is the stored, idempotent result of a decision. Replaying the
// same request_id returns this record unchanged. RequestDigest lets the state
// machine detect a reused key carrying different request content (a conflict).
type DecisionRecord struct {
	SchemaVersion  uint32 `json:"schema_version"`
	RequestID      string `json:"request_id"`
	RequestDigest  string `json:"request_digest"`
	ClientID       string `json:"client_id"`
	PolicyID       string `json:"policy_id"`
	Subject        string `json:"subject"`
	CostTokens     int64  `json:"cost_tokens"`
	Allowed        bool   `json:"allowed"`
	RemainingMilli int64  `json:"remaining_milli"`
	RetryAfterMS   int64  `json:"retry_after_ms"`
	ObservedAtMS   int64  `json:"observed_at_ms"`
	ExpiresAtMS    int64  `json:"expires_at_ms"`
	LeaderTerm     uint64 `json:"leader_term"`
	LogIndex       uint64 `json:"log_index"`
}

// AuditEvent is an append-only record keyed by committed log index. It never
// contains raw keys, tokens, cookies, or full subjects — the subject is stored
// only as a short one-way hash.
type AuditEvent struct {
	SchemaVersion  uint32 `json:"schema_version"`
	EventType      string `json:"event_type"`
	TimestampMS    int64  `json:"timestamp_ms"`
	ActorType      string `json:"actor_type"`
	ActorID        string `json:"actor_id"`
	RequestID      string `json:"request_id"`
	PolicyID       string `json:"policy_id"`
	SubjectHash    string `json:"subject_hash"`
	Allowed        bool   `json:"allowed"`
	CostTokens     int64  `json:"cost_tokens"`
	RemainingMilli int64  `json:"remaining_milli"`
	Term           uint64 `json:"term"`
	LogIndex       uint64 `json:"log_index"`
}

// --- encode/decode helpers (JSON in schema version 1) ---

func encodePolicy(p Policy) ([]byte, error)           { return json.Marshal(p) }
func encodeClient(c Client) ([]byte, error)           { return json.Marshal(c) }
func encodeBucket(b TokenBucketState) ([]byte, error) { return json.Marshal(b) }
func encodeDecision(d DecisionRecord) ([]byte, error) { return json.Marshal(d) }
func encodeAudit(a AuditEvent) ([]byte, error)        { return json.Marshal(a) }

func decodePolicy(b []byte) (Policy, error) {
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return Policy{}, fmt.Errorf("limiter: decode policy: %w", err)
	}
	return p, nil
}

func decodeClient(b []byte) (Client, error) {
	var c Client
	if err := json.Unmarshal(b, &c); err != nil {
		return Client{}, fmt.Errorf("limiter: decode client: %w", err)
	}
	return c, nil
}

// DecodeClient decodes a stored client record. Exported so the auth layer can
// read client records for API-key verification.
func DecodeClient(b []byte) (Client, error) { return decodeClient(b) }

// DecodePolicy decodes a stored policy record. Exported so the admin API can
// list and render policies.
func DecodePolicy(b []byte) (Policy, error) { return decodePolicy(b) }

func decodeBucket(b []byte) (TokenBucketState, error) {
	var s TokenBucketState
	if err := json.Unmarshal(b, &s); err != nil {
		return TokenBucketState{}, fmt.Errorf("limiter: decode bucket: %w", err)
	}
	return s, nil
}

func decodeDecision(b []byte) (DecisionRecord, error) {
	var d DecisionRecord
	if err := json.Unmarshal(b, &d); err != nil {
		return DecisionRecord{}, fmt.Errorf("limiter: decode decision: %w", err)
	}
	return d, nil
}
