package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
)

type fakeAdminStore struct {
	policies [][]byte
	clients  [][]byte
}

func (f *fakeAdminStore) Policies() ([][]byte, error)         { return f.policies, nil }
func (f *fakeAdminStore) Clients() ([][]byte, error)          { return f.clients, nil }
func (f *fakeAdminStore) Policy(string) ([]byte, bool, error) { return nil, false, nil }

var testSessionKey = []byte("session-key-0123456789abcdef0123")

func adminServer(t *testing.T, node ProposerNode, store AdminStore) (*httptest.Server, *auth.SessionManager) {
	t.Helper()
	sm := auth.NewSessionManager(testSessionKey, false)
	authn := auth.NewAPIKeyAuthenticator([]byte("pepper-0123456789abcdef012345678"), nil)
	if store == nil {
		store = &fakeAdminStore{}
	}
	h := NewAdminHandler(sm, node, authn, store, nil)
	mux := http.NewServeMux()
	RegisterAdmin(mux, h)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, sm
}

// session mints a valid session cookie and returns it with its CSRF token.
func session(t *testing.T, sm *auth.SessionManager) (*http.Cookie, string) {
	t.Helper()
	sess, err := sm.Issue()
	if err != nil {
		t.Fatal(err)
	}
	return sm.SessionCookie(sess), sess.CSRFToken
}

func adminReq(t *testing.T, method, url string, cookie *http.Cookie, csrf, origin string, body []byte) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body != nil {
		r, err = http.NewRequest(method, url, bytes.NewReader(body))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAdminJSONRequiresSession(t *testing.T) {
	srv, _ := adminServer(t, &fakeProposer{}, nil)
	resp := adminReq(t, http.MethodGet, srv.URL+"/api/admin/policies", nil, "", "", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session status = %d, want 401", resp.StatusCode)
	}
}

func TestAdminMutationRequiresCSRFAndOrigin(t *testing.T) {
	srv, sm := adminServer(t, &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeApplied, Policy: &limiter.Policy{ID: "pol"}}}, nil)
	cookie, csrf := session(t, sm)
	body := []byte(`{"id":"pol","name":"Policy","capacity_tokens":5,"refill_tokens":1,"refill_interval_ms":12000,"max_cost_tokens":1,"active":true}`)

	// Session but no CSRF -> 403.
	resp := adminReq(t, http.MethodPost, srv.URL+"/api/admin/policies", cookie, "", srv.URL, body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("missing CSRF status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// CSRF but wrong origin -> 403.
	resp = adminReq(t, http.MethodPost, srv.URL+"/api/admin/policies", cookie, csrf, "https://evil.example.com", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad origin status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Session + CSRF + same origin -> 201.
	resp = adminReq(t, http.MethodPost, srv.URL+"/api/admin/policies", cookie, csrf, srv.URL, body)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("valid status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("missing no-store header")
	}
	_ = resp.Body.Close()
}

func TestAdminCreatePolicyDuplicate(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectPolicyExists}}
	srv, sm := adminServer(t, node, nil)
	cookie, csrf := session(t, sm)
	body := []byte(`{"id":"pol","name":"Policy","capacity_tokens":5,"refill_tokens":1,"refill_interval_ms":12000,"max_cost_tokens":1,"active":true}`)
	resp := adminReq(t, http.MethodPost, srv.URL+"/api/admin/policies", cookie, csrf, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp.StatusCode)
	}
}

func TestAdminUpdatePolicyVersionConflict(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectVersionConflict}}
	srv, sm := adminServer(t, node, nil)
	cookie, csrf := session(t, sm)
	body := []byte(`{"name":"P","capacity_tokens":5,"refill_tokens":1,"refill_interval_ms":12000,"max_cost_tokens":1,"expected_version":1}`)
	resp := adminReq(t, http.MethodPut, srv.URL+"/api/admin/policies/pol", cookie, csrf, srv.URL, body)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("version conflict status = %d, want 409", resp.StatusCode)
	}
}

func TestAdminCreateClientReturnsRawKeyOnce(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeApplied, Client: &limiter.Client{
		ID: "cli_x", Name: "svc", KeyPrefix: "TESTPFX1", AllowedPolicyIDs: []string{"*"}, Active: true,
	}}}
	srv, sm := adminServer(t, node, nil)
	cookie, csrf := session(t, sm)

	resp := adminReq(t, http.MethodPost, srv.URL+"/api/admin/clients", cookie, csrf, srv.URL, []byte(`{"name":"svc","allowed_policy_ids":["*"]}`))
	defer func() { _ = resp.Body.Close() }()
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
	if bytes.Contains(node.command, []byte(out.APIKey)) {
		t.Error("raw API key leaked into the replicated command")
	}
}

func TestAdminRevokeClientAlreadyRevoked(t *testing.T) {
	node := &fakeProposer{result: limiter.Result{Outcome: limiter.OutcomeApplied, Duplicate: true, Client: &limiter.Client{ID: "cli_x", Active: false}}}
	srv, sm := adminServer(t, node, nil)
	cookie, csrf := session(t, sm)

	resp := adminReq(t, http.MethodPost, srv.URL+"/api/admin/clients/cli_x/revoke", cookie, csrf, srv.URL, []byte(`{"confirm":true}`))
	defer func() { _ = resp.Body.Close() }()
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

func TestAdminListPolicies(t *testing.T) {
	pol := limiter.Policy{SchemaVersion: 1, ID: "pol", Name: "Policy", Active: true, Version: 1}
	pb, _ := json.Marshal(pol)
	srv, sm := adminServer(t, &fakeProposer{}, &fakeAdminStore{policies: [][]byte{pb}})
	cookie, _ := session(t, sm)

	resp := adminReq(t, http.MethodGet, srv.URL+"/api/admin/policies", cookie, "", "", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var out []limiter.Policy
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out) != 1 || out[0].ID != "pol" {
		t.Errorf("policies = %+v", out)
	}
}
