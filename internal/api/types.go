package api

// DecisionRequest is the public decision request body.
type DecisionRequest struct {
	PolicyID string `json:"policy_id"`
	Subject  string `json:"subject"`
	Cost     int64  `json:"cost"`
}

// DecisionResponse is returned for both allowed (200) and denied (429)
// decisions. RequestID here is the decision's identity (the idempotency key);
// the trace id is carried separately in the X-Request-ID header.
type DecisionResponse struct {
	RequestID      string `json:"request_id"`
	PolicyID       string `json:"policy_id"`
	Allowed        bool   `json:"allowed"`
	Cost           int64  `json:"cost"`
	RemainingMilli int64  `json:"remaining_milli"`
	RetryAfterMS   int64  `json:"retry_after_ms"`
	ObservedAt     string `json:"observed_at"`
	LeaderTerm     uint64 `json:"leader_term"`
	LogIndex       uint64 `json:"log_index"`
	Duplicate      bool   `json:"duplicate"`
}
