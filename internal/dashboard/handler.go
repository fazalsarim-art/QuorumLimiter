package dashboard

import (
	"context"
	"crypto/subtle"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

const (
	flashCookie      = "ql_flash"
	recentAuditLimit = 100
)

// Node is the consensus surface the dashboard needs.
type Node interface {
	Status() (raft.Status, error)
	Propose(ctx context.Context, command []byte) (any, error)
}

// Store is the read surface the dashboard needs.
type Store interface {
	Policies() ([][]byte, error)
	Clients() ([][]byte, error)
	RecentAudits(limit int) ([][]byte, error)
}

// Handler renders the admin dashboard and processes its form mutations.
type Handler struct {
	sessions        *auth.SessionManager
	adminToken      string
	throttle        *loginThrottle
	node            Node
	authn           *auth.APIKeyAuthenticator
	store           Store
	rd              *renderer
	baseURL         string
	proposalTimeout time.Duration
	log             *slog.Logger
	now             func() int64
}

// New builds a dashboard handler.
func New(sessions *auth.SessionManager, adminToken string, node Node, authn *auth.APIKeyAuthenticator, store Store, baseURL string, proposalTimeout time.Duration, logger *slog.Logger) (*Handler, error) {
	rd, err := newRenderer()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if proposalTimeout <= 0 {
		proposalTimeout = 3 * time.Second
	}
	return &Handler{
		sessions:        sessions,
		adminToken:      adminToken,
		throttle:        newLoginThrottle(5, time.Minute),
		node:            node,
		authn:           authn,
		store:           store,
		rd:              rd,
		baseURL:         strings.TrimRight(baseURL, "/"),
		proposalTimeout: proposalTimeout,
		log:             logger.With(slog.String("component", "dashboard")),
		now:             func() int64 { return time.Now().UnixMilli() },
	}, nil
}

// Register attaches the dashboard routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/login", h.loginGet)
	mux.HandleFunc("POST /admin/login", h.loginPost)
	mux.HandleFunc("POST /admin/logout", h.form(h.logoutPost))

	mux.HandleFunc("GET /admin", h.page("overview", "Overview", "overview", h.buildOverview))
	mux.HandleFunc("GET /admin/policies", h.page("policies", "Policies", "policies", h.buildPolicies))
	mux.HandleFunc("GET /admin/clients", h.page("clients", "API clients", "clients", h.buildClients))
	mux.HandleFunc("GET /admin/decisions", h.page("decisions", "Decisions", "decisions", h.buildDecisions))
	mux.HandleFunc("GET /admin/cluster", h.page("cluster", "Cluster", "cluster", h.buildCluster))
	mux.HandleFunc("GET /admin/docs", h.page("docs", "API docs", "docs", h.buildDocs))

	mux.HandleFunc("POST /admin/policies", h.form(h.createPolicy))
	mux.HandleFunc("POST /admin/policies/{id}/update", h.form(h.updatePolicy))
	mux.HandleFunc("POST /admin/policies/{id}/activation", h.form(h.activation))
	mux.HandleFunc("POST /admin/clients", h.form(h.createClient))
	mux.HandleFunc("POST /admin/clients/{id}/revoke", h.form(h.revokeClient))
	mux.HandleFunc("POST /admin/test-decisions", h.form(h.testDecision))

	static, _ := fs.Sub(staticFS, ".")
	mux.Handle("GET /admin/static/", http.StripPrefix("/admin/", http.FileServerFS(static)))
}

// --- security + session helpers ---

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'")
}

// page wraps an authenticated GET page: it requires a session (redirecting to
// login otherwise), builds the view model, and renders.
func (h *Handler) page(nav, title, tmpl string, build func(*http.Request, auth.Session) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		sess, err := h.sessions.FromRequest(r)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		data, berr := build(r, sess)
		pd := h.pageData(w, title, nav, sess, r)
		if berr != nil {
			pd.Flash = &flash{Level: "error", Message: "Some data could not be read."}
		}
		pd.Data = data
		h.rd.render(w, http.StatusOK, tmpl, pd)
	}
}

// form wraps a mutating POST: session + same-origin + CSRF form token.
func (h *Handler) form(next func(http.ResponseWriter, *http.Request, auth.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		sess, err := h.sessions.FromRequest(r)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !auth.SameOrigin(r) || !auth.VerifyCSRFToken(sess, r.PostFormValue("csrf_token")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r, sess)
	}
}

func (h *Handler) pageData(w http.ResponseWriter, title, nav string, sess auth.Session, r *http.Request) pageData {
	pd := pageData{Title: title, Nav: nav, Authed: true, CSRFToken: sess.CSRFToken, Flash: h.takeFlash(w, r)}
	if s, err := h.node.Status(); err == nil {
		pd.TopBar = topBar{NodeID: s.NodeID, Role: s.Role.String(), Term: s.Term, LeaderID: s.LeaderID}
	}
	return pd
}

// --- login / logout ---

func (h *Handler) loginGet(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	if _, err := h.sessions.FromRequest(r); err == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	h.rd.render(w, http.StatusOK, "login", pageData{Title: "Sign in", Data: loginData{}})
}

func (h *Handler) loginPost(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)
	ip := clientIP(r)
	if h.throttle.blocked(ip) {
		h.rd.render(w, http.StatusTooManyRequests, "login", pageData{Title: "Sign in", Data: loginData{Error: true}})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("admin_token")), []byte(h.adminToken)) != 1 {
		h.throttle.fail(ip)
		h.rd.render(w, http.StatusUnauthorized, "login", pageData{Title: "Sign in", Data: loginData{Error: true}})
		return
	}
	h.throttle.reset(ip)
	sess, err := h.sessions.Issue()
	if err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, h.sessions.SessionCookie(sess))
	http.SetCookie(w, h.sessions.CSRFCookie(sess))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) logoutPost(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	for _, c := range h.sessions.ClearCookies() {
		http.SetCookie(w, c)
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// --- page builders ---

func (h *Handler) buildOverview(_ *http.Request, _ auth.Session) (any, error) {
	d := overviewData{}
	if s, err := h.node.Status(); err == nil {
		d.LeaderID, d.Term, d.Role = s.LeaderID, s.Term, s.Role.String()
		d.CommitIndex, d.LastApplied = s.CommitIndex, s.LastApplied
	}
	if raw, err := h.store.Policies(); err == nil {
		for _, p := range decodePolicies(raw) {
			d.PolicyCount++
			if p.Active {
				d.Policies = append(d.Policies, p)
			}
		}
	}
	if raw, err := h.store.Clients(); err == nil {
		d.ClientCount = len(raw)
	}
	if raw, err := h.store.RecentAudits(recentAuditLimit); err == nil {
		for _, b := range raw {
			a, derr := limiter.DecodeAudit(b)
			if derr != nil || a.EventType != "decision" {
				continue
			}
			d.RecentTotal++
			if a.Allowed {
				d.AllowedRecent++
			} else {
				d.DeniedRecent++
			}
		}
	}
	return d, nil
}

func (h *Handler) buildPolicies(r *http.Request, _ auth.Session) (any, error) {
	status := r.URL.Query().Get("status")
	q := strings.ToLower(r.URL.Query().Get("q"))
	raw, err := h.store.Policies()
	if err != nil {
		return policiesData{Status: status, Query: r.URL.Query().Get("q")}, err
	}
	var out []limiter.Policy
	for _, p := range decodePolicies(raw) {
		if status == "active" && !p.Active || status == "inactive" && p.Active {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(p.Name), q) && !strings.Contains(strings.ToLower(p.ID), q) {
			continue
		}
		out = append(out, p)
	}
	return policiesData{Policies: out, Status: status, Query: r.URL.Query().Get("q")}, nil
}

func (h *Handler) buildClients(_ *http.Request, _ auth.Session) (any, error) {
	raw, err := h.store.Clients()
	if err != nil {
		return clientsData{}, err
	}
	return clientsData{Clients: decodeClients(raw)}, nil
}

func (h *Handler) buildDecisions(_ *http.Request, _ auth.Session) (any, error) {
	raw, err := h.store.RecentAudits(recentAuditLimit)
	if err != nil {
		return decisionsData{}, err
	}
	audits := make([]limiter.AuditEvent, 0, len(raw))
	for _, b := range raw {
		if a, derr := limiter.DecodeAudit(b); derr == nil {
			audits = append(audits, a)
		}
	}
	return decisionsData{Audits: audits}, nil
}

func (h *Handler) buildCluster(_ *http.Request, _ auth.Session) (any, error) {
	s, err := h.node.Status()
	if err != nil {
		return clusterData{}, err
	}
	return clusterData{Status: s}, nil
}

func (h *Handler) buildDocs(_ *http.Request, _ auth.Session) (any, error) {
	base := h.baseURL
	if base == "" {
		base = "http://localhost:8080"
	}
	return docsData{BaseURL: base}, nil
}

// --- form mutations ---

func (h *Handler) createPolicy(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	payload := limiter.CreatePolicyPayload{
		ID:               r.PostFormValue("id"),
		Name:             r.PostFormValue("name"),
		CapacityTokens:   formInt(r, "capacity_tokens"),
		RefillTokens:     formInt(r, "refill_tokens"),
		RefillIntervalMS: formInt(r, "refill_interval_ms"),
		MaxCostTokens:    formInt(r, "max_cost_tokens"),
		Active:           r.PostFormValue("active") == "true",
	}
	res, err := h.proposeCmd(r.Context(), limiter.CmdCreatePolicy, payload)
	h.flashResult(w, r, "/admin/policies", res, err, "Policy created.")
}

func (h *Handler) updatePolicy(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	payload := limiter.UpdatePolicyPayload{
		ID:               r.PathValue("id"),
		Name:             r.PostFormValue("name"),
		CapacityTokens:   formInt(r, "capacity_tokens"),
		RefillTokens:     formInt(r, "refill_tokens"),
		RefillIntervalMS: formInt(r, "refill_interval_ms"),
		MaxCostTokens:    formInt(r, "max_cost_tokens"),
		ExpectedVersion:  uint64(formInt(r, "expected_version")),
	}
	res, err := h.proposeCmd(r.Context(), limiter.CmdUpdatePolicy, payload)
	h.flashResult(w, r, "/admin/policies", res, err, "Policy updated.")
}

func (h *Handler) activation(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	payload := limiter.SetPolicyActivePayload{
		ID:              r.PathValue("id"),
		Active:          r.PostFormValue("active") == "true",
		ExpectedVersion: uint64(formInt(r, "expected_version")),
	}
	res, err := h.proposeCmd(r.Context(), limiter.CmdSetPolicyActive, payload)
	msg := "Policy activated."
	if !payload.Active {
		msg = "Policy deactivated."
	}
	h.flashResult(w, r, "/admin/policies", res, err, msg)
}

func (h *Handler) createClient(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	name := r.PostFormValue("name")
	var allowed []string
	if r.PostFormValue("allow_all") == "true" {
		allowed = []string{"*"}
	} else {
		for _, p := range strings.Split(r.PostFormValue("allowed_policy_ids"), ",") {
			if s := strings.TrimSpace(p); s != "" {
				allowed = append(allowed, s)
			}
		}
	}
	rawKey, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		h.setFlash(w, "error", "Key generation failed.")
		http.Redirect(w, r, "/admin/clients", http.StatusSeeOther)
		return
	}
	clientID, err := auth.GenerateClientID()
	if err != nil {
		h.setFlash(w, "error", "ID generation failed.")
		http.Redirect(w, r, "/admin/clients", http.StatusSeeOther)
		return
	}
	res, perr := h.proposeCmd(r.Context(), limiter.CmdCreateClient, limiter.CreateClientPayload{
		ID: clientID, Name: name, KeyPrefix: prefix, KeyDigest: h.authn.Digest(rawKey), AllowedPolicyIDs: allowed,
	})
	if perr != nil {
		h.setFlash(w, "error", "Cluster unavailable; retry.")
		http.Redirect(w, r, "/admin/clients", http.StatusSeeOther)
		return
	}
	if res.Outcome != limiter.OutcomeApplied {
		h.setFlash(w, "error", "Could not create client: "+res.RejectMsg)
		http.Redirect(w, r, "/admin/clients", http.StatusSeeOther)
		return
	}
	// Success: render the clients page immediately with the one-time key.
	raw, _ := h.store.Clients()
	pd := h.pageData(w, "API clients", "clients", sess, r)
	pd.Data = clientsData{Clients: decodeClients(raw), NewKey: rawKey, NewClientID: clientID}
	h.rd.render(w, http.StatusCreated, "clients", pd)
}

func (h *Handler) revokeClient(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if r.PostFormValue("confirm") != "true" {
		h.setFlash(w, "error", "Revocation not confirmed.")
		http.Redirect(w, r, "/admin/clients", http.StatusSeeOther)
		return
	}
	res, err := h.proposeCmd(r.Context(), limiter.CmdRevokeClient, limiter.RevokeClientPayload{ID: r.PathValue("id")})
	h.flashResult(w, r, "/admin/clients", res, err, "Client revoked.")
}

func (h *Handler) testDecision(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	reqID, _ := auth.GenerateClientID() // reuse as a unique idempotency key source
	payload := limiter.DecidePayload{
		ClientID: "admin", PolicyID: r.PostFormValue("policy_id"), Subject: r.PostFormValue("subject"),
		Cost: formInt(r, "cost"), RequestID: "admintest-" + reqID, ActorType: "admin",
	}
	res, err := h.proposeCmd(r.Context(), limiter.CmdDecide, payload)
	if err != nil {
		h.setFlash(w, "error", "Cluster unavailable; retry.")
	} else if res.Decision != nil {
		if res.Decision.Allowed {
			h.setFlash(w, "success", "Allowed. Remaining: "+tokensStr(res.Decision.RemainingMilli))
		} else {
			h.setFlash(w, "info", "Denied. Retry after "+strconv.FormatInt(res.Decision.RetryAfterMS, 10)+"ms")
		}
	} else {
		h.setFlash(w, "error", "Rejected: "+res.RejectMsg)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// --- propose + flash helpers ---

func (h *Handler) proposeCmd(ctx context.Context, typ limiter.CommandType, payload any) (limiter.Result, error) {
	cmd, err := limiter.NewCommand(genID(), typ, h.now(), payload)
	if err != nil {
		return limiter.Result{}, err
	}
	body, err := cmd.Encode()
	if err != nil {
		return limiter.Result{}, err
	}
	pctx, cancel := context.WithTimeout(ctx, h.proposalTimeout)
	defer cancel()
	res, err := h.node.Propose(pctx, body)
	if err != nil {
		return limiter.Result{}, err
	}
	r, ok := res.(limiter.Result)
	if !ok {
		return limiter.Result{}, errors.New("unexpected result type")
	}
	return r, nil
}

func (h *Handler) flashResult(w http.ResponseWriter, r *http.Request, redirect string, res limiter.Result, err error, okMsg string) {
	switch {
	case err != nil:
		h.setFlash(w, "error", "Cluster unavailable; retry.")
	case res.Outcome == limiter.OutcomeApplied:
		h.setFlash(w, "success", okMsg)
	default:
		h.setFlash(w, "error", "Rejected: "+res.RejectMsg)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (h *Handler) setFlash(w http.ResponseWriter, level, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(level + "|" + msg), Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 30,
	})
}

// takeFlash reads and clears the flash cookie. It needs the ResponseWriter to
// clear the cookie; pageData passes a nil writer for pages that were reached
// without one, so clearing is best-effort.
func (h *Handler) takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	if w != nil {
		http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/admin", MaxAge: -1})
	}
	decoded, _ := url.QueryUnescape(c.Value)
	parts := strings.SplitN(decoded, "|", 2)
	if len(parts) != 2 {
		return nil
	}
	return &flash{Level: parts[0], Message: parts[1]}
}

// --- small utils ---

func formInt(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue(key)), 10, 64)
	return v
}

func tokensStr(milli int64) string {
	return itoa(milli/1000) + "." + pad3(milli%1000)
}

func genID() string {
	id, err := auth.GenerateClientID()
	if err != nil {
		return "cmd"
	}
	return "cmd_" + id
}

func clientIP(r *http.Request) string {
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}

// loginThrottle bounds failed sign-ins per source IP.
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
	if len(t.failures) > 10000 {
		t.failures = make(map[string][]int64)
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
