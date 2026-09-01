package limiter

import (
	"encoding/json"
	"fmt"
)

// CommandType identifies a replicated command.
type CommandType string

const (
	CmdCreatePolicy    CommandType = "create_policy"
	CmdUpdatePolicy    CommandType = "update_policy"
	CmdSetPolicyActive CommandType = "set_policy_active"
	CmdCreateClient    CommandType = "create_client"
	CmdRevokeClient    CommandType = "revoke_client"
	CmdDecide          CommandType = "decide"
	CmdPrune           CommandType = "prune"
)

// Command is the versioned envelope stored in a Raft log entry's Command field.
// TimestampMS is the leader-observed time; the state machine uses only this,
// never a local clock, so all nodes compute identical results.
type Command struct {
	SchemaVersion uint32          `json:"schema_version"`
	ID            string          `json:"id"`
	Type          CommandType     `json:"type"`
	TimestampMS   int64           `json:"timestamp_ms"`
	Payload       json.RawMessage `json:"payload"`
}

// Encode marshals a command to bytes for storage in a log entry.
func (c Command) Encode() ([]byte, error) { return json.Marshal(c) }

// DecodeCommand parses a command envelope.
func DecodeCommand(b []byte) (Command, error) {
	var c Command
	if err := json.Unmarshal(b, &c); err != nil {
		return Command{}, fmt.Errorf("limiter: decode command: %w", err)
	}
	return c, nil
}

// NewCommand builds a command envelope with the given typed payload.
func NewCommand(id string, typ CommandType, timestampMS int64, payload any) (Command, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Command{}, fmt.Errorf("limiter: encode %s payload: %w", typ, err)
	}
	return Command{
		SchemaVersion: ModelSchemaVersion,
		ID:            id,
		Type:          typ,
		TimestampMS:   timestampMS,
		Payload:       raw,
	}, nil
}

// --- payloads ---

// CreatePolicyPayload creates a new policy.
type CreatePolicyPayload struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	CapacityTokens   int64  `json:"capacity_tokens"`
	RefillTokens     int64  `json:"refill_tokens"`
	RefillIntervalMS int64  `json:"refill_interval_ms"`
	MaxCostTokens    int64  `json:"max_cost_tokens"`
	Active           bool   `json:"active"`
}

// UpdatePolicyPayload edits an existing policy. ExpectedVersion guards against
// concurrent edits (optimistic concurrency).
type UpdatePolicyPayload struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	CapacityTokens   int64  `json:"capacity_tokens"`
	RefillTokens     int64  `json:"refill_tokens"`
	RefillIntervalMS int64  `json:"refill_interval_ms"`
	MaxCostTokens    int64  `json:"max_cost_tokens"`
	ExpectedVersion  uint64 `json:"expected_version"`
}

// SetPolicyActivePayload activates or deactivates a policy.
type SetPolicyActivePayload struct {
	ID              string `json:"id"`
	Active          bool   `json:"active"`
	ExpectedVersion uint64 `json:"expected_version"`
}

// CreateClientPayload creates an API client. The raw key is generated on the
// leader before proposing; only the prefix and HMAC digest are replicated.
type CreateClientPayload struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	KeyPrefix        string   `json:"key_prefix"`
	KeyDigest        []byte   `json:"key_digest"`
	AllowedPolicyIDs []string `json:"allowed_policy_ids"`
}

// RevokeClientPayload revokes an API client.
type RevokeClientPayload struct {
	ID string `json:"id"`
}

// DecidePayload requests a rate-limit decision. RequestID is the idempotency
// key; replaying it returns the original result without charging twice.
type DecidePayload struct {
	ClientID  string `json:"client_id"`
	PolicyID  string `json:"policy_id"`
	Subject   string `json:"subject"`
	Cost      int64  `json:"cost"`
	RequestID string `json:"request_id"`
	ActorType string `json:"actor_type"` // "client" or "admin"
}

// PrunePayload removes expired idempotency records and trims audits. The state
// machine uses the command timestamp as the cutoff, never a local clock.
type PrunePayload struct {
	AuditKeepMax int `json:"audit_keep_max"`
}

func unmarshalPayload(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("limiter: decode payload: %w", err)
	}
	return nil
}
