package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

const testAdminToken = "admin-token-abcdefghij"

type fakeNode struct {
	status  raft.Status
	result  limiter.Result
	err     error
	command []byte
}

func (f *fakeNode) Status() (raft.Status, error) { return f.status, nil }
func (f *fakeNode) Propose(_ context.Context, cmd []byte) (any, error) {
	f.command = cmd
	return f.result, f.err
}

type fakeStore struct {
	policies [][]byte
	clients  [][]byte
	audits   [][]byte
}

func (f *fakeStore) Policies() ([][]byte, error)        { return f.policies, nil }
func (f *fakeStore) Clients() ([][]byte, error)         { return f.clients, nil }
func (f *fakeStore) RecentAudits(int) ([][]byte, error) { return f.audits, nil }

func newServer(t *testing.T, node Node, store Store) *httptest.Server {
	t.Helper()
	sm := auth.NewSessionManager([]byte("session-key-0123456789abcdef0123"), false)
	authn := auth.NewAPIKeyAuthenticator([]byte("pepper-0123456789abcdef012345678"), nil)
	if store == nil {
		store = &fakeStore{}
	}
	h, err := New(sm, testAdminToken, node, authn, store, "http://localhost:8080", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func noRedirect(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func login(t *testing.T, srv *httptest.Server) (*http.Client, string) {
	t.Helper()
	c := noRedirect(t)
	resp, err := c.PostForm(srv.URL+"/admin/login", url.Values{"admin_token": {testAdminToken}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	u, _ := url.Parse(srv.URL)
	var csrf string
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.CSRFCookieName {
			csrf = ck.Value
		}
	}
	if csrf == "" {
		t.Fatal("no CSRF cookie after login")
	}
	return c, csrf
}

func TestDashboardUnauthRedirectsToLogin(t *testing.T) {
	srv := newServer(t, &fakeNode{}, nil)
	c := noRedirect(t)
	for _, path := range []string{"/admin", "/admin/policies", "/admin/clients", "/admin/cluster"} {
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || !strings.HasSuffix(resp.Header.Get("Location"), "/admin/login") {
			t.Errorf("%s: status=%d loc=%q, want 302 -> /admin/login", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestDashboardLoginWrongToken(t *testing.T) {
	srv := newServer(t, &fakeNode{}, nil)
	c := noRedirect(t)
	resp, err := c.PostForm(srv.URL+"/admin/login", url.Values{"admin_token": {"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401", resp.StatusCode)
	}
}

func TestDashboardPolicyPageEscapesAndHasCSRF(t *testing.T) {
	// A policy whose name contains HTML must be escaped in the rendered page.
	pol := limiter.Policy{SchemaVersion: 1, ID: "pol", Name: "<script>x</script>", Active: true, Version: 1}
	pb, _ := json.Marshal(pol)
	srv := newServer(t, &fakeNode{status: raft.Status{NodeID: "node1", Role: raft.RoleLeader}}, &fakeStore{policies: [][]byte{pb}})
	c, csrf := login(t, srv)

	resp, err := c.Get(srv.URL + "/admin/policies")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if strings.Contains(html, "<script>x</script>") {
		t.Error("policy name was not HTML-escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("expected escaped policy name in output")
	}
	if !strings.Contains(html, csrf) {
		t.Error("expected CSRF token embedded in forms")
	}
}

func TestDashboardCreatePolicyForm(t *testing.T) {
	node := &fakeNode{
		status: raft.Status{NodeID: "node1", Role: raft.RoleLeader},
		result: limiter.Result{Outcome: limiter.OutcomeApplied, Policy: &limiter.Policy{ID: "pol"}},
	}
	srv := newServer(t, node, nil)
	c, csrf := login(t, srv)

	form := url.Values{
		"csrf_token": {csrf}, "id": {"pol"}, "name": {"Password reset"},
		"capacity_tokens": {"5"}, "refill_tokens": {"1"}, "refill_interval_ms": {"12000"},
		"max_cost_tokens": {"1"}, "active": {"true"},
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/policies", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create policy status = %d, want 303", resp.StatusCode)
	}
	if node.command == nil {
		t.Error("create policy did not propose a command")
	}
}

func TestDashboardFormRequiresCSRF(t *testing.T) {
	srv := newServer(t, &fakeNode{status: raft.Status{Role: raft.RoleLeader}}, nil)
	c, _ := login(t, srv)

	// Missing csrf_token -> 403.
	form := url.Values{"id": {"pol"}, "name": {"P"}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/policies", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("missing CSRF status = %d, want 403", resp.StatusCode)
	}
}

func TestDashboardCreateClientShowsKeyOnce(t *testing.T) {
	node := &fakeNode{
		status: raft.Status{NodeID: "node1", Role: raft.RoleLeader},
		result: limiter.Result{Outcome: limiter.OutcomeApplied, Client: &limiter.Client{ID: "cli_x", Name: "svc", Active: true, AllowedPolicyIDs: []string{"*"}}},
	}
	srv := newServer(t, node, nil)
	c, csrf := login(t, srv)

	form := url.Values{"csrf_token": {csrf}, "name": {"svc"}, "allow_all": {"true"}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/clients", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create client status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("client key page must be no-store")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "qlk_") {
		t.Error("expected the one-time raw key in the response")
	}
	// The raw key must not have been placed into the replicated command.
	start := strings.Index(string(body), "qlk_")
	rawKey := string(body)[start : start+20]
	if node.command != nil && strings.Contains(string(node.command), rawKey) {
		t.Error("raw key leaked into replicated command")
	}
}

func TestDashboardStaticServed(t *testing.T) {
	srv := newServer(t, &fakeNode{}, nil)
	resp, err := http.Get(srv.URL + "/admin/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("static css status = %d, want 200", resp.StatusCode)
	}
}
