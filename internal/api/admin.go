package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

// AdminStore is the read surface the admin JSON API needs. *storage.Store
// satisfies it.
type AdminStore interface {
	Policies() ([][]byte, error)
	Clients() ([][]byte, error)
	Policy(id string) ([]byte, bool, error)
}

// AdminHandler serves the Raft-backed admin JSON APIs under /api/admin. Session
// sign-in itself lives in the dashboard package; these handlers only validate an
// existing session (and CSRF for mutations).
type AdminHandler struct {
	sessions *auth.SessionManager
	node     ProposerNode
	authn    *auth.APIKeyAuthenticator
	store    AdminStore
	log      *slog.Logger
	now      func() int64
}

// NewAdminHandler builds an admin JSON handler.
func NewAdminHandler(sessions *auth.SessionManager, node ProposerNode, authn *auth.APIKeyAuthenticator, store AdminStore, logger *slog.Logger) *AdminHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AdminHandler{
		sessions: sessions,
		node:     node,
		authn:    authn,
		store:    store,
		log:      logger.With(slog.String("component", "admin-api")),
		now:      func() int64 { return time.Now().UnixMilli() },
	}
}

// AdminSecurityHeaders sets the security + no-store headers used on every admin
// response (shared with the dashboard).
func AdminSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'")
}

// requireAdmin wraps a JSON admin handler, enforcing a valid session and, for
// mutating requests, same-origin and a valid CSRF token.
func (h *AdminHandler) requireAdmin(mutating bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		AdminSecurityHeaders(w)
		sess, err := h.sessions.FromRequest(r)
		if err != nil {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "admin session required", "")
			return
		}
		if mutating {
			if !auth.SameOrigin(r) {
				writeAPIError(w, http.StatusForbidden, "forbidden", "cross-origin request rejected", "")
				return
			}
			if !auth.VerifyCSRFToken(sess, r.Header.Get("X-CSRF-Token")) {
				writeAPIError(w, http.StatusForbidden, "forbidden", "invalid CSRF token", "")
				return
			}
		}
		next(w, r)
	}
}

// --- policy API ---

type createPolicyRequest struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	CapacityTokens   int64  `json:"capacity_tokens"`
	RefillTokens     int64  `json:"refill_tokens"`
	RefillIntervalMS int64  `json:"refill_interval_ms"`
	MaxCostTokens    int64  `json:"max_cost_tokens"`
	Active           bool   `json:"active"`
}

func (h *AdminHandler) listPolicies(w http.ResponseWriter, r *http.Request) {
	raw, err := h.store.Policies()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "could not read policies", "")
		return
	}
	status := r.URL.Query().Get("status")
	q := strings.ToLower(r.URL.Query().Get("q"))
	out := make([]limiter.Policy, 0, len(raw))
	for _, b := range raw {
		p, derr := limiter.DecodePolicy(b)
		if derr != nil {
			continue
		}
		if status == "active" && !p.Active || status == "inactive" && p.Active {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(p.Name), q) && !strings.Contains(strings.ToLower(p.ID), q) {
			continue
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *AdminHandler) createPolicy(w http.ResponseWriter, r *http.Request) {
	var req createPolicyRequest
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body", "")
		return
	}
	cmd, err := limiter.NewCommand(generateID("acmd_"), limiter.CmdCreatePolicy, h.now(), limiter.CreatePolicyPayload{
		ID: req.ID, Name: req.Name, CapacityTokens: req.CapacityTokens, RefillTokens: req.RefillTokens,
		RefillIntervalMS: req.RefillIntervalMS, MaxCostTokens: req.MaxCostTokens, Active: req.Active,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "encode command", "")
		return
	}
	res, perr := h.propose(r.Context(), cmd)
	if perr != nil {
		h.mapProposeError(w, perr)
		return
	}
	switch {
	case res.Outcome == limiter.OutcomeApplied:
		writeJSON(w, http.StatusCreated, res.Policy)
	case res.RejectCode == limiter.RejectPolicyExists:
		writeAPIError(w, http.StatusConflict, "policy_exists", res.RejectMsg, "")
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", res.RejectMsg, "")
	}
}

type updatePolicyRequest struct {
	Name             string `json:"name"`
	CapacityTokens   int64  `json:"capacity_tokens"`
	RefillTokens     int64  `json:"refill_tokens"`
	RefillIntervalMS int64  `json:"refill_interval_ms"`
	MaxCostTokens    int64  `json:"max_cost_tokens"`
	ExpectedVersion  uint64 `json:"expected_version"`
}

func (h *AdminHandler) updatePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req updatePolicyRequest
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body", "")
		return
	}
	cmd, err := limiter.NewCommand(generateID("acmd_"), limiter.CmdUpdatePolicy, h.now(), limiter.UpdatePolicyPayload{
		ID: id, Name: req.Name, CapacityTokens: req.CapacityTokens, RefillTokens: req.RefillTokens,
		RefillIntervalMS: req.RefillIntervalMS, MaxCostTokens: req.MaxCostTokens, ExpectedVersion: req.ExpectedVersion,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "encode command", "")
		return
	}
	res, perr := h.propose(r.Context(), cmd)
	if perr != nil {
		h.mapProposeError(w, perr)
		return
	}
	switch {
	case res.Outcome == limiter.OutcomeApplied:
		writeJSON(w, http.StatusOK, res.Policy)
	case res.RejectCode == limiter.RejectPolicyNotFound:
		writeAPIError(w, http.StatusNotFound, "policy_not_found", res.RejectMsg, "")
	case res.RejectCode == limiter.RejectVersionConflict:
		writeAPIError(w, http.StatusConflict, "version_conflict", res.RejectMsg, "")
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", res.RejectMsg, "")
	}
}

type activationRequest struct {
	Active          bool   `json:"active"`
	ExpectedVersion uint64 `json:"expected_version"`
}

func (h *AdminHandler) changeActivation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req activationRequest
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body", "")
		return
	}
	cmd, err := limiter.NewCommand(generateID("acmd_"), limiter.CmdSetPolicyActive, h.now(), limiter.SetPolicyActivePayload{
		ID: id, Active: req.Active, ExpectedVersion: req.ExpectedVersion,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "encode command", "")
		return
	}
	res, perr := h.propose(r.Context(), cmd)
	if perr != nil {
		h.mapProposeError(w, perr)
		return
	}
	switch {
	case res.Outcome == limiter.OutcomeApplied:
		writeJSON(w, http.StatusOK, res.Policy)
	case res.RejectCode == limiter.RejectPolicyNotFound:
		writeAPIError(w, http.StatusNotFound, "policy_not_found", res.RejectMsg, "")
	case res.RejectCode == limiter.RejectVersionConflict:
		writeAPIError(w, http.StatusConflict, "version_conflict", res.RejectMsg, "")
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", res.RejectMsg, "")
	}
}

// --- client API ---

type clientView struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	KeyPrefix        string   `json:"key_prefix"`
	AllowedPolicyIDs []string `json:"allowed_policy_ids"`
	Active           bool     `json:"active"`
	CreatedAt        string   `json:"created_at"`
	RevokedAt        *string  `json:"revoked_at"`
}

func toClientView(c limiter.Client) clientView {
	v := clientView{
		ID: c.ID, Name: c.Name, KeyPrefix: c.KeyPrefix,
		AllowedPolicyIDs: c.AllowedPolicyIDs, Active: c.Active,
		CreatedAt: msToRFC3339(c.CreatedAtMS),
	}
	if c.RevokedAtMS != nil {
		s := msToRFC3339(*c.RevokedAtMS)
		v.RevokedAt = &s
	}
	return v
}

func (h *AdminHandler) listClients(w http.ResponseWriter, r *http.Request) {
	raw, err := h.store.Clients()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "could not read clients", "")
		return
	}
	status := r.URL.Query().Get("status")
	out := make([]clientView, 0, len(raw))
	for _, b := range raw {
		c, derr := limiter.DecodeClient(b)
		if derr != nil {
			continue
		}
		if status == "active" && !c.Active || status == "revoked" && c.Active {
			continue
		}
		out = append(out, toClientView(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type createClientRequest struct {
	Name             string   `json:"name"`
	AllowedPolicyIDs []string `json:"allowed_policy_ids"`
}

func (h *AdminHandler) createClient(w http.ResponseWriter, r *http.Request) {
	var req createClientRequest
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body", "")
		return
	}
	rawKey, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "key generation failed", "")
		return
	}
	clientID, err := auth.GenerateClientID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "id generation failed", "")
		return
	}
	cmd, err := limiter.NewCommand(generateID("acmd_"), limiter.CmdCreateClient, h.now(), limiter.CreateClientPayload{
		ID: clientID, Name: req.Name, KeyPrefix: prefix, KeyDigest: h.authn.Digest(rawKey),
		AllowedPolicyIDs: req.AllowedPolicyIDs,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "encode command", "")
		return
	}
	res, perr := h.propose(r.Context(), cmd)
	if perr != nil {
		h.mapProposeError(w, perr)
		return
	}
	switch {
	case res.Outcome == limiter.OutcomeApplied:
		writeJSON(w, http.StatusCreated, map[string]any{
			"client":  toClientView(*res.Client),
			"api_key": rawKey,
		})
	case res.RejectCode == limiter.RejectPolicyNotFound:
		writeAPIError(w, http.StatusNotFound, "policy_not_found", res.RejectMsg, "")
	case res.RejectCode == limiter.RejectKeyPrefixConflict, res.RejectCode == limiter.RejectClientExists:
		writeAPIError(w, http.StatusConflict, "conflict", res.RejectMsg, "")
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", res.RejectMsg, "")
	}
}

type revokeClientRequest struct {
	Confirm bool `json:"confirm"`
}

func (h *AdminHandler) revokeClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req revokeClientRequest
	if err := decodeAdminBody(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "malformed JSON body", "")
		return
	}
	if !req.Confirm {
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", "confirm must be true", "")
		return
	}
	cmd, err := limiter.NewCommand(generateID("acmd_"), limiter.CmdRevokeClient, h.now(), limiter.RevokeClientPayload{ID: id})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "encode command", "")
		return
	}
	res, perr := h.propose(r.Context(), cmd)
	if perr != nil {
		h.mapProposeError(w, perr)
		return
	}
	switch {
	case res.Outcome == limiter.OutcomeApplied:
		writeJSON(w, http.StatusOK, map[string]any{
			"client":          toClientView(*res.Client),
			"already_revoked": res.Duplicate,
		})
	case res.RejectCode == limiter.RejectClientNotFound:
		writeAPIError(w, http.StatusNotFound, "client_not_found", res.RejectMsg, "")
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "validation_failed", res.RejectMsg, "")
	}
}

// --- helpers ---

func (h *AdminHandler) propose(ctx context.Context, cmd limiter.Command) (limiter.Result, error) {
	body, err := cmd.Encode()
	if err != nil {
		return limiter.Result{}, err
	}
	res, err := h.node.Propose(ctx, body)
	if err != nil {
		return limiter.Result{}, err
	}
	r, ok := res.(limiter.Result)
	if !ok {
		return limiter.Result{}, errors.New("unexpected result type")
	}
	return r, nil
}

func (h *AdminHandler) mapProposeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, raft.ErrProposalTimeout), errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "proposal_timeout", "timed out awaiting commit", "")
	default:
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusServiceUnavailable, "leader_unavailable", "no leader or quorum; retry", "")
	}
}

func decodeAdminBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxDecisionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing data")
	}
	return nil
}
