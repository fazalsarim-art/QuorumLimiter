package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/auth"
	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

const (
	testPepper = "decide-pepper-0123456789abcdef01"
	testAPIKey = "qlk_TESTPFX1_secretvalue1234567890"
	testIdem   = "idem-000000000001"
)

// fakeProposer is a canned ProposerNode.
type fakeProposer struct {
	status  raft.Status
	result  any
	err     error
	command []byte
}

func (f *fakeProposer) Status() (raft.Status, error) { return f.status, nil }
func (f *fakeProposer) Propose(_ context.Context, cmd []byte) (any, error) {
	f.command = cmd
	return f.result, f.err
}

// fakeClientReader satisfies auth.ClientReader for a single test client.
type fakeClientReader struct {
	prefixToID map[string]string
	idToBytes  map[string][]byte
}

func (f *fakeClientReader) ClientIDByPrefix(p string) (string, bool, error) {
	id, ok := f.prefixToID[p]
	return id, ok, nil
}
func (f *fakeClientReader) Client(id string) ([]byte, bool, error) {
	b, ok := f.idToBytes[id]
	return b, ok, nil
}

func testAuthenticator(t *testing.T, allowed []string, active bool) *auth.APIKeyAuthenticator {
	t.Helper()
	pepper := []byte(testPepper)
	digest := auth.NewAPIKeyAuthenticator(pepper, nil).Digest(testAPIKey)
	client := limiter.Client{
		ID: "cli_1", Name: "Test", KeyPrefix: "TESTPFX1", KeyDigest: digest,
		AllowedPolicyIDs: allowed, Active: active,
	}
	body, _ := json.Marshal(client)
	cr := &fakeClientReader{
		prefixToID: map[string]string{"TESTPFX1": "cli_1"},
		idToBytes:  map[string][]byte{"cli_1": body},
	}
	return auth.NewAPIKeyAuthenticator(pepper, cr)
}

func decisionServer(t *testing.T, node ProposerNode, authn *auth.APIKeyAuthenticator, peerURLs map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	h := NewDecisionHandler(node, authn, "node1", peerURLs, 0, nil, nil)
	RegisterDecision(mux, h)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

type decideOpts struct {
	key         string
	idem        string
	contentType string
	forwarded   bool
	body        []byte
}

func postDecision(t *testing.T, url string, o decideOpts) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/decisions", bytes.NewReader(o.body))
	if err != nil {
		t.Fatal(err)
	}
	if o.contentType != "" {
		req.Header.Set("Content-Type", o.contentType)
	}
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	if o.idem != "" {
		req.Header.Set(idempotencyHeader, o.idem)
	}
	if o.forwarded {
		req.Header.Set(forwardedHeader, "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func validBody() []byte {
	b, _ := json.Marshal(DecisionRequest{PolicyID: "pol", Subject: "s", Cost: 1})
	return b
}

func leaderStatus() raft.Status { return raft.Status{NodeID: "node1", Role: raft.RoleLeader, Term: 3} }

func TestDecisionAllowed(t *testing.T) {
	node := &fakeProposer{
		status: leaderStatus(),
		result: limiter.Result{Outcome: limiter.OutcomeAllowed, Decision: &limiter.DecisionRecord{
			RequestID: testIdem, PolicyID: "pol", Allowed: true, CostTokens: 1,
			RemainingMilli: 4000, ObservedAtMS: 1_700_000_000_000, LeaderTerm: 3, LogIndex: 42,
		}},
	}
	s := decisionServer(t, node, testAuthenticator(t, []string{"*"}, true), nil)

	resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get(requestIDHeader) == "" {
		t.Error("missing X-Request-ID header")
	}
	var out DecisionResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !out.Allowed || out.RemainingMilli != 4000 || out.LogIndex != 42 {
		t.Errorf("response = %+v", out)
	}
}

func TestDecisionDenied(t *testing.T) {
	node := &fakeProposer{
		status: leaderStatus(),
		result: limiter.Result{Outcome: limiter.OutcomeDenied, Decision: &limiter.DecisionRecord{
			RequestID: testIdem, PolicyID: "pol", Allowed: false, CostTokens: 1,
			RemainingMilli: 200, RetryAfterMS: 9360, ObservedAtMS: 1_700_000_000_000,
		}},
	}
	s := decisionServer(t, node, testAuthenticator(t, []string{"*"}, true), nil)

	resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "10" { // ceil(9360ms)
		t.Errorf("Retry-After = %q, want 10", resp.Header.Get("Retry-After"))
	}
}

func TestDecisionRejectMappings(t *testing.T) {
	cases := []struct {
		name string
		res  limiter.Result
		want int
	}{
		{"policy not found", limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectPolicyNotFound}, http.StatusNotFound},
		{"validation", limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectValidation}, http.StatusUnprocessableEntity},
		{"idempotency conflict", limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectIdempotencyConflict}, http.StatusConflict},
		{"inactive policy", limiter.Result{Outcome: limiter.OutcomeRejected, RejectCode: limiter.RejectPolicyInactive}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &fakeProposer{status: leaderStatus(), result: tc.res}
			s := decisionServer(t, node, testAuthenticator(t, []string{"*"}, true), nil)
			resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestDecisionInputErrors(t *testing.T) {
	node := &fakeProposer{status: leaderStatus(), result: limiter.Result{Outcome: limiter.OutcomeAllowed, Decision: &limiter.DecisionRecord{}}}
	s := decisionServer(t, node, testAuthenticator(t, []string{"*"}, true), nil)

	cases := []struct {
		name string
		o    decideOpts
		want int
	}{
		{"missing key", decideOpts{idem: testIdem, contentType: "application/json", body: validBody()}, http.StatusUnauthorized},
		{"bad key", decideOpts{key: "qlk_TESTPFX1_wrong", idem: testIdem, contentType: "application/json", body: validBody()}, http.StatusUnauthorized},
		{"wrong content type", decideOpts{key: testAPIKey, idem: testIdem, contentType: "text/plain", body: validBody()}, http.StatusUnsupportedMediaType},
		{"missing idempotency", decideOpts{key: testAPIKey, contentType: "application/json", body: validBody()}, http.StatusUnprocessableEntity},
		{"malformed json", decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: []byte("{oops")}, http.StatusBadRequest},
		{"unknown field", decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: []byte(`{"nope":1}`)}, http.StatusBadRequest},
		{"bad subject", decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: mustJSON(DecisionRequest{PolicyID: "pol", Subject: "bad subject!", Cost: 1})}, http.StatusUnprocessableEntity},
		{"short idempotency", decideOpts{key: testAPIKey, idem: "short", contentType: "application/json", body: validBody()}, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postDecision(t, s.URL, tc.o)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestDecisionForbiddenPolicy(t *testing.T) {
	node := &fakeProposer{status: leaderStatus()}
	// client allowed only "other", not "pol"
	s := decisionServer(t, node, testAuthenticator(t, []string{"other"}, true), nil)
	resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if node.command != nil {
		t.Error("forbidden request must not be proposed")
	}
}

func TestDecisionProposalTimeout(t *testing.T) {
	node := &fakeProposer{status: leaderStatus(), err: raft.ErrProposalTimeout}
	s := decisionServer(t, node, testAuthenticator(t, []string{"*"}, true), nil)
	resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
}

func TestDecisionNoLeader(t *testing.T) {
	node := &fakeProposer{status: raft.Status{NodeID: "node1", Role: raft.RoleFollower, LeaderID: ""}}
	s := decisionServer(t, node, testAuthenticator(t, []string{"*"}, true), nil)
	resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 should carry Retry-After")
	}
}

func TestDecisionForwardsToLeader(t *testing.T) {
	// Leader server processes and allows.
	leaderNode := &fakeProposer{
		status: leaderStatus(),
		result: limiter.Result{Outcome: limiter.OutcomeAllowed, Decision: &limiter.DecisionRecord{
			RequestID: testIdem, PolicyID: "pol", Allowed: true, RemainingMilli: 3000, LogIndex: 7,
		}},
	}
	leaderSrv := decisionServer(t, leaderNode, testAuthenticator(t, []string{"*"}, true), nil)

	// Follower forwards to the leader.
	followerNode := &fakeProposer{status: raft.Status{NodeID: "node1", Role: raft.RoleFollower, LeaderID: "node2"}}
	followerSrv := decisionServer(t, followerNode, testAuthenticator(t, []string{"*"}, true), map[string]string{"node2": leaderSrv.URL})

	resp := postDecision(t, followerSrv.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forwarded status = %d, want 200", resp.StatusCode)
	}
	if followerNode.command != nil {
		t.Error("follower must not propose; it should forward")
	}
	if leaderNode.command == nil {
		t.Error("leader should have received the forwarded proposal")
	}
	var out DecisionResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.LogIndex != 7 {
		t.Errorf("relayed body LogIndex = %d, want 7", out.LogIndex)
	}
}

func TestDecisionForwardLoopPrevented(t *testing.T) {
	followerNode := &fakeProposer{status: raft.Status{NodeID: "node1", Role: raft.RoleFollower, LeaderID: "node2"}}
	s := decisionServer(t, followerNode, testAuthenticator(t, []string{"*"}, true), map[string]string{"node2": "http://unused"})

	// An already-forwarded request on a non-leader must not forward again.
	resp := postDecision(t, s.URL, decideOpts{key: testAPIKey, idem: testIdem, contentType: "application/json", forwarded: true, body: validBody()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (loop prevented)", resp.StatusCode)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
