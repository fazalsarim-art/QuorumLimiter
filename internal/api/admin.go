package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

// Login throttling: at most this many failures per source IP per window.
const (
	maxLoginFailures = 5
	loginWindow      = time.Minute
	maxThrottleIPs   = 10000
)

// AdminStore is the read surface the admin API needs. *storage.Store satisfies it.
type AdminStore interface {
	Policies() ([][]byte, error)
	Clients() ([][]byte, error)
	Policy(id string) ([]byte, bool, error)
}

// AdminHandler serves admin sign-in and the Raft-backed policy/client APIs.
type AdminHandler struct {
	sessions   *auth.SessionManager
	adminToken string
	throttle   *loginThrottle
	node       ProposerNode
	authn      *auth.APIKeyAuthenticator
	store      AdminStore
	log        *slog.Logger
	now        func() int64
}

// NewAdminHandler builds an admin handler.
func NewAdminHandler(sessions *auth.SessionManager, adminToken string, node ProposerNode, authn *auth.APIKeyAuthenticator, store AdminStore, logger *slog.Logger) *AdminHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AdminHandler{
		sessions:   sessions,
		adminToken: adminToken,
		throttle:   newLoginThrottle(maxLoginFailures, loginWindow),
		node:       node,
		authn:      authn,
		store:      store,
		log:        logger.With(slog.String("component", "admin")),
		now:        func() int64 { return time.Now().UnixMilli() },
	}
}

// --- security headers ---

func adminHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'")
}

// --- authentication middleware ---

// requireAdmin wraps a JSON admin handler, enforcing a valid session and, for
// mutating requests, same-origin and a valid CSRF token.
func (h *AdminHandler) requireAdmin(mutating bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminHeaders(w)
		if _, err := h.sessions.FromRequest(r); err != nil {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "admin session required", "")
			return
		}
		if mutating {
			sess, _ := h.sessions.FromRequest(r)
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

// --- login / logout ---

const loginPageHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">` +
	`<meta name="viewport" content="width=device-width, initial-scale=1">` +
	`<title>QuorumLimiter admin</title></head><body><h1>Admin sign in</h1>` +
	`<form method="post" action="/admin/login">` +
	`<label>Admin token <input type="password" name="admin_token" autocomplete="off"></label> ` +
	`<button type="submit">Sign in</button></form></body></html>`

func (h *AdminHandler) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	adminHeaders(w)
	if _, err := h.sessions.FromRequest(r); err == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginPageHTML))
}

func (h *AdminHandler) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	adminHeaders(w)
	ip := clientIP(r)
	if h.throttle.blocked(ip) {
		writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in attempts", "")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "could not parse form", "")
		return
	}
	token := r.PostFormValue("admin_token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(h.adminToken)) != 1 {
		h.throttle.fail(ip)
		// Generic failure: never reveal whether the token was close.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(loginPageHTML))
		return
	}
	h.throttle.reset(ip)
	sess, err := h.sessions.Issue()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "could not create session", "")
		return
	}
	http.SetCookie(w, h.sessions.SessionCookie(sess))
	http.SetCookie(w, h.sessions.CSRFCookie(sess))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *AdminHandler) handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	adminHeaders(w)
	sess, err := h.sessions.FromRequest(r)
	if err != nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if !auth.SameOrigin(r) || !auth.VerifyCSRFToken(sess, r.Header.Get("X-CSRF-Token")) {
		writeAPIError(w, http.StatusForbidden, "forbidden", "invalid CSRF token or origin", "")
		return
	}
	for _, c := range h.sessions.ClearCookies() {
		http.SetCookie(w, c)
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
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
	// Generate the raw key on the leader; only prefix + digest are replicated.
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
		// Return the raw key exactly once (adminHeaders already set no-store).
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

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- login throttle ---

type loginThrottle struct {
	mu       sync.Mutex
	failures map[string][]int64
	max      int
	window   time.Duration
	now      func() time.Time
}

func newLoginThrottle(max int, window time.Duration) *loginThrottle {
	return &loginThrottle{failures: make(map[string][]int64), max: max, window: window, now: time.Now}
}

func (t *loginThrottle) fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.failures) > maxThrottleIPs {
		t.failures = make(map[string][]int64) // bound memory: reset under flood
	}
	now := t.now().UnixMilli()
	t.failures[ip] = append(t.prune(t.failures[ip], now), now)
}

func (t *loginThrottle) blocked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UnixMilli()
	recent := t.prune(t.failures[ip], now)
	t.failures[ip] = recent
	return len(recent) >= t.max
}

func (t *loginThrottle) reset(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, ip)
}

func (t *loginThrottle) prune(times []int64, now int64) []int64 {
	cutoff := now - t.window.Milliseconds()
	kept := times[:0]
	for _, ts := range times {
		if ts >= cutoff {
			kept = append(kept, ts)
		}
	}
	return kept
}
