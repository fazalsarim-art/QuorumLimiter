package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

const (
	maxDecisionBody = 64 << 10 // 64 KiB
	forwardTimeout  = 2 * time.Second
)

// ProposerNode is the subset of *raft.Node the decision handler needs.
type ProposerNode interface {
	Status() (raft.Status, error)
	Propose(ctx context.Context, command []byte) (any, error)
}

// DecisionHandler serves POST /v1/decisions. On a follower it forwards to the
// leader; on the leader it authenticates, proposes a decision command, and waits
// for the committed apply result.
type DecisionHandler struct {
	node            ProposerNode
	auth            *auth.APIKeyAuthenticator
	selfID          string
	peerURLs        map[string]string // node ID -> advertised base URL
	proposalTimeout time.Duration
	forwardClient   *http.Client
	now             func() int64 // leader-observed time in ms (injectable)
	log             *slog.Logger
}

// NewDecisionHandler builds a decision handler.
func NewDecisionHandler(node ProposerNode, authn *auth.APIKeyAuthenticator, selfID string, peerURLs map[string]string, proposalTimeout time.Duration, logger *slog.Logger) *DecisionHandler {
	if logger == nil {
		logger = slog.Default()
	}
	if proposalTimeout <= 0 {
		proposalTimeout = 3 * time.Second
	}
	return &DecisionHandler{
		node:            node,
		auth:            authn,
		selfID:          selfID,
		peerURLs:        peerURLs,
		proposalTimeout: proposalTimeout,
		forwardClient:   &http.Client{Timeout: forwardTimeout},
		now:             func() int64 { return time.Now().UnixMilli() },
		log:             logger.With(slog.String("component", "decision")),
	}
}

func (h *DecisionHandler) handleDecision(w http.ResponseWriter, r *http.Request) {
	reqID := requestID(r)
	w.Header().Set(requestIDHeader, reqID)

	status, err := h.node.Status()
	if err != nil {
		h.retryable(w, reqID, "node unavailable")
		return
	}

	if status.Role == raft.RoleLeader {
		h.processLocally(w, r, reqID)
		return
	}

	// Follower: forward once to the known leader.
	if r.Header.Get(forwardedHeader) != "" {
		// Already forwarded; refuse to forward again to prevent loops.
		h.retryable(w, reqID, "not leader after forward")
		return
	}
	if status.LeaderID == "" {
		h.retryable(w, reqID, "no leader elected")
		return
	}
	leaderURL, ok := h.peerURLs[status.LeaderID]
	if !ok {
		h.retryable(w, reqID, "unknown leader address")
		return
	}
	h.forward(w, r, leaderURL, reqID)
}

func (h *DecisionHandler) processLocally(w http.ResponseWriter, r *http.Request, reqID string) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "content-type must be application/json", reqID)
		return
	}
	idem := r.Header.Get(idempotencyHeader)
	if idem == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", "Idempotency-Key header is required", reqID)
		return
	}
	rawKey, ok := bearerToken(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "missing bearer token", reqID)
		return
	}
	client, err := h.auth.Authenticate(rawKey)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidAPIKey) {
			writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid api key", reqID)
			return
		}
		h.retryable(w, reqID, "auth unavailable")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDecisionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req DecisionRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large", reqID)
			return
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body", reqID)
		return
	}
	if dec.More() {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "unexpected trailing data", reqID)
		return
	}

	if err := limiter.ValidateDecisionInput(req.PolicyID, req.Subject, idem, req.Cost); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error(), reqID)
		return
	}
	if !client.AllowsPolicy(req.PolicyID) {
		writeAPIError(w, http.StatusForbidden, "forbidden_policy", "client may not use this policy", reqID)
		return
	}

	cmd, err := limiter.NewCommand(generateID("dcmd_"), limiter.CmdDecide, h.now(), limiter.DecidePayload{
		ClientID: client.ID, PolicyID: req.PolicyID, Subject: req.Subject,
		Cost: req.Cost, RequestID: idem, ActorType: "client",
	})
	if err != nil {
		h.retryable(w, reqID, "encode command")
		return
	}
	body, err := cmd.Encode()
	if err != nil {
		h.retryable(w, reqID, "encode command")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.proposalTimeout)
	defer cancel()
	res, perr := h.node.Propose(ctx, body)
	if perr != nil {
		h.mapProposeError(w, perr, reqID)
		return
	}
	result, ok := res.(limiter.Result)
	if !ok {
		h.retryable(w, reqID, "unexpected result")
		return
	}
	h.writeResult(w, result, reqID)
}

func (h *DecisionHandler) writeResult(w http.ResponseWriter, result limiter.Result, reqID string) {
	if result.Outcome == limiter.OutcomeRejected {
		h.writeReject(w, result, reqID)
		return
	}
	d := result.Decision
	if d == nil {
		h.retryable(w, reqID, "missing decision")
		return
	}
	resp := DecisionResponse{
		RequestID: d.RequestID, PolicyID: d.PolicyID, Allowed: d.Allowed, Cost: d.CostTokens,
		RemainingMilli: d.RemainingMilli, RetryAfterMS: d.RetryAfterMS,
		ObservedAt: msToRFC3339(d.ObservedAtMS), LeaderTerm: d.LeaderTerm, LogIndex: d.LogIndex,
		Duplicate: result.Duplicate,
	}
	if d.Allowed {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(ceilSeconds(d.RetryAfterMS)))
	writeJSON(w, http.StatusTooManyRequests, resp)
}

func (h *DecisionHandler) writeReject(w http.ResponseWriter, result limiter.Result, reqID string) {
	switch result.RejectCode {
	case limiter.RejectValidation:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", result.RejectMsg, reqID)
	case limiter.RejectPolicyNotFound, limiter.RejectPolicyInactive:
		writeAPIError(w, http.StatusNotFound, "policy_not_found", result.RejectMsg, reqID)
	case limiter.RejectForbiddenPolicy:
		writeAPIError(w, http.StatusForbidden, "forbidden_policy", result.RejectMsg, reqID)
	case limiter.RejectClientNotFound, limiter.RejectClientRevoked:
		writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid api key", reqID)
	case limiter.RejectIdempotencyConflict:
		writeAPIError(w, http.StatusConflict, "idempotency_conflict", result.RejectMsg, reqID)
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", result.RejectMsg, reqID)
	}
}

func (h *DecisionHandler) mapProposeError(w http.ResponseWriter, err error, reqID string) {
	switch {
	case errors.Is(err, raft.ErrProposalTimeout), errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "proposal_timeout",
			"timed out awaiting commit; retry with the same Idempotency-Key", reqID)
	case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrNoLeader),
		errors.Is(err, raft.ErrLostQuorum), errors.Is(err, raft.ErrStopped):
		h.retryable(w, reqID, "no leader or quorum")
	default:
		h.retryable(w, reqID, "unavailable")
	}
}

// retryable writes a 503 with a Retry-After hint.
func (h *DecisionHandler) retryable(w http.ResponseWriter, reqID, msg string) {
	w.Header().Set("Retry-After", "1")
	writeAPIError(w, http.StatusServiceUnavailable, "leader_unavailable", msg, reqID)
}

// forward relays the request to the leader over the private node network,
// preserving authorization and idempotency and marking it to prevent loops.
func (h *DecisionHandler) forward(w http.ResponseWriter, r *http.Request, leaderURL, reqID string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDecisionBody))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "could not read request body", reqID)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), forwardTimeout)
	defer cancel()

	fReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(leaderURL, "/")+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		h.retryable(w, reqID, "build forward request")
		return
	}
	// Preserve authorization and idempotency; mark as forwarded; drop everything
	// else (no hop-by-hop headers).
	if v := r.Header.Get("Authorization"); v != "" {
		fReq.Header.Set("Authorization", v)
	}
	// Preserve the original Content-Type so the leader validates it identically
	// whether the request arrived directly or via forwarding.
	if v := r.Header.Get("Content-Type"); v != "" {
		fReq.Header.Set("Content-Type", v)
	}
	if v := r.Header.Get(idempotencyHeader); v != "" {
		fReq.Header.Set(idempotencyHeader, v)
	}
	fReq.Header.Set(requestIDHeader, reqID)
	fReq.Header.Set(forwardedHeader, "1")

	resp, err := h.forwardClient.Do(fReq)
	if err != nil {
		h.retryable(w, reqID, "leader forward failed")
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if ra := resp.Header.Get("Retry-After"); ra != "" {
		w.Header().Set("Retry-After", ra)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func ceilSeconds(ms int64) int {
	if ms <= 0 {
		return 0
	}
	return int((ms + 999) / 1000)
}

func msToRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
