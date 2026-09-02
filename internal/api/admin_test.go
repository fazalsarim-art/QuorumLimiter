package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
)

const testAdminToken = "admin-token-abcdefghij"

type fakeAdminStore struct {
	policies [][]byte
	clients  [][]byte
}

func (f *fakeAdminStore) Policies() ([][]byte, error) { return f.policies, nil }
func (f *fakeAdminStore) Clients() ([][]byte, error)  { return f.clients, nil }
func (f *fakeAdminStore) Policy(string) ([]byte, bool, error) {
	return nil, false, nil
}

func adminServer(t *testing.T, node ProposerNode, store AdminStore) *httptest.Server {
	t.Helper()
	sm := auth.NewSessionManager([]byte("session-key-0123456789abcdef0123"), false)
	authn := auth.NewAPIKeyAuthenticator([]byte("pepper-0123456789abcdef012345678"), nil)
	if store == nil {
		store = &fakeAdminStore{}
	}
	h := NewAdminHandler(sm, testAdminToken, node, authn, store, nil)
	mux := http.NewServeMux()
	RegisterAdmin(mux, h)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// loginAdmin signs in and returns a cookie-jar client plus the CSRF token.
func loginAdmin(t *testing.T, srv *httptest.Server) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(srv.URL+"/admin/login", url.Values{"admin_token": {testAdminToken}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	u, _ := url.Parse(srv.URL)
	var csrf string
	for _, ck := range jar.Cookies(u) {
		if ck.Name == auth.CSRFCookieName {
			csrf = ck.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF cookie after login")
	}
	return c, csrf
}

func adminPost(t *testing.T, c *http.Client, srv *httptest.Server, path, csrf string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.Header.Set("Origin", srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdminLoginSuccessAndWrongToken(t *testing.T) {
	srv := adminServer(t, &fakeProposer{}, nil)

	// Wrong token -> 401.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := c.PostForm(srv.URL+"/admin/login", url.Values{"admin_token": {"nope"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct token -> 303 + cookies.
	_, csrf := loginAdmin(t, srv)
	if csrf == "" {
		t.Error("expected CSRF token after login")
	}
}

func TestAdminLoginThrottled(t *testing.T) {
	srv := adminServer(t, &fakeProposer{}, nil)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// 5 failures allowed, the 6th is throttled.
	var last int
	for i := 0; i < 6; i++ {
		resp, _ := c.PostForm(srv.URL+"/admin/login", url.Values{"admin_token": {"wrong"}})
		last = resp.StatusCode
		resp.Body.Close()
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("6th attempt status = %d, want 429", last)
	}
}

func TestAdminMutationRequiresSessionAndCSRF(t *testing.T) {
	srv := adminServer(t, &fakeProposer{}, nil)
	body := []byte(`{"id":"pol","name":"Policy","capacity_tokens":5,"refill_tokens":1,"refill_interval_ms":12000,"max_cost_tokens":1,"active":true}`)

	// No session -> 401.
	noSession := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/policies", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	resp, _ := noSession.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	c, csrf := loginAdmin(t, srv)

	// Session but no CSRF header -> 403.
	resp = adminPost(t, c, srv, "/api/admin/policies", "", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("missing CSRF status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Session + CSRF but wrong Origin -> 403.
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/policies", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-CSRF-Token", csrf)
	req2.Header.Set("Origin", "https://evil.example.com")
	resp, _ = c.Do(req2)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad origin status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminCreatePolicy(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeApplied, Policy: &limiter.Policy{ID: "pol", Version: 1}}}
	srv := adminServer(t, node, nil)
	c, csrf := loginAdmin(t, srv)

	body := []byte(`{"id":"pol","name":"Policy","capacity_tokens":5,"refill_tokens":1,"refill_interval_ms":12000,"max_cost_tokens":1,"active":true}`)
	resp := adminPost(t, c, srv, "/api/admin/policies", csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("missing no-store header")
	}

	// Duplicate -> 409.
	node.result = limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectPolicyExists}
	resp2 := adminPost(t, c, srv, "/api/admin/policies", csrf, body)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp2.StatusCode)
	}
}

func TestAdminCreateClientReturnsRawKeyOnce(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeApplied, Client: &limiter.Client{
		ID: "cli_x", Name: "svc", KeyPrefix: "TESTPFX1", AllowedPolicyIDs: []string{"*"}, Active: true,
	}}}
	srv := adminServer(t, node, nil)
	c, csrf := loginAdmin(t, srv)

	resp := adminPost(t, c, srv, "/api/admin/clients", csrf, []byte(`{"name":"svc","allowed_policy_ids":["*"]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("client creation must be no-store")
	}
	var out struct {
		Client clientView `json:"client"`
		APIKey string     `json:"api_key"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.HasPrefix(out.APIKey, "qlk_") {
		t.Errorf("api_key = %q, want qlk_ prefix", out.APIKey)
	}
	// The proposed command must not contain the raw secret (only prefix+digest).
	if bytes.Contains(node.command, []byte(out.APIKey)) {
		t.Error("raw API key leaked into the replicated command")
	}
}

func TestAdminRevokeClientAlreadyRevoked(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeApplied, Duplicate: true, Client: &limiter.Client{ID: "cli_x", Active: false}}}
	srv := adminServer(t, node, nil)
	c, csrf := loginAdmin(t, srv)

	resp := adminPost(t, c, srv, "/api/admin/clients/cli_x/revoke", csrf, []byte(`{"confirm":true}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		AlreadyRevoked bool `json:"already_revoked"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !out.AlreadyRevoked {
		t.Error("already_revoked = false, want true")
	}
}

func TestAdminListPoliciesRequiresSession(t *testing.T) {
	pol := limiter.Policy{SchemaVersion: 1, ID: "pol", Name: "Policy", Active: true, Version: 1}
	pb, _ := json.Marshal(pol)
	srv := adminServer(t, &fakeProposer{}, &fakeAdminStore{policies: [][]byte{pb}})

	// Unauthenticated -> 401.
	resp, _ := http.Get(srv.URL + "/api/admin/policies")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth list status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticated -> 200 with the policy.
	c, _ := loginAdmin(t, srv)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/policies", nil)
	resp2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp2.StatusCode)
	}
	var out []limiter.Policy
	_ = json.NewDecoder(resp2.Body).Decode(&out)
	if len(out) != 1 || out[0].ID != "pol" {
		t.Errorf("policies = %+v", out)
	}
}
