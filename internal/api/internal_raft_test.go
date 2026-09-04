package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

const testToken = "cluster-token-abcdefghij"

// fakeRaftNode is a canned RaftNode for handler tests.
type fakeRaftNode struct {
	voteResp   raft.RequestVoteResponse
	gotVote    raft.RequestVoteRequest
	appendResp raft.AppendEntriesResponse
	status     raft.Status
}

func (f *fakeRaftNode) HandleRequestVote(_ context.Context, req raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	f.gotVote = req
	return f.voteResp, nil
}

func (f *fakeRaftNode) HandleAppendEntries(_ context.Context, _ raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	return f.appendResp, nil
}

func (f *fakeRaftNode) Status() (raft.Status, error) { return f.status, nil }

func testServer(t *testing.T, node RaftNode, allowed []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	h := NewInternalRaftHandler(node, testToken, allowed, nil)
	RegisterInternalRaft(mux, h)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func postVote(t *testing.T, url, token, contentType string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/internal/raft/request-vote", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func voteBody(t *testing.T, source string) []byte {
	t.Helper()
	b, _ := json.Marshal(raft.RequestVoteRequest{
		ProtocolVersion: raft.ProtocolVersion, ClusterID: "test-cluster",
		SourceNodeID: source, Term: 5, CandidateID: source,
	})
	return b
}

func TestInternalRequestVoteSuccess(t *testing.T) {
	node := &fakeRaftNode{voteResp: raft.RequestVoteResponse{Term: 5, VoteGranted: true, SourceNodeID: "node2"}}
	s := testServer(t, node, []string{"node1"})

	resp := postVote(t, s.URL, testToken, "application/json", voteBody(t, "node1"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got raft.RequestVoteResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if !got.VoteGranted || got.Term != 5 {
		t.Errorf("response = %+v", got)
	}
	if node.gotVote.CandidateID != "node1" {
		t.Errorf("node saw candidate %q", node.gotVote.CandidateID)
	}
}

func TestInternalAuthAndValidation(t *testing.T) {
	node := &fakeRaftNode{}
	s := testServer(t, node, []string{"node1"})

	cases := []struct {
		name        string
		token       string
		contentType string
		source      string
		body        []byte
		want        int
	}{
		{"no token", "", "application/json", "node1", voteBody(t, "node1"), http.StatusUnauthorized},
		{"wrong token", "nope", "application/json", "node1", voteBody(t, "node1"), http.StatusUnauthorized},
		{"wrong content type", testToken, "text/plain", "node1", voteBody(t, "node1"), http.StatusUnsupportedMediaType},
		{"forbidden source", testToken, "application/json", "node9", voteBody(t, "node9"), http.StatusForbidden},
		{"malformed json", testToken, "application/json", "node1", []byte("{not json"), http.StatusBadRequest},
		{"unknown field", testToken, "application/json", "node1", []byte(`{"nope":1}`), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postVote(t, s.URL, tc.token, tc.contentType, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestInternalBodyTooLarge(t *testing.T) {
	node := &fakeRaftNode{}
	s := testServer(t, node, []string{"node1"})

	huge := append([]byte(`{"candidate_id":"`), bytes.Repeat([]byte("a"), 70<<10)...)
	huge = append(huge, []byte(`"}`)...)
	resp := postVote(t, s.URL, testToken, "application/json", huge)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestInternalStatus(t *testing.T) {
	node := &fakeRaftNode{status: raft.Status{NodeID: "node2", Role: raft.RoleFollower, Term: 7}}
	s := testServer(t, node, []string{"node1"})

	req, _ := http.NewRequest(http.MethodGet, s.URL+"/internal/raft/status", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got raft.Status
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.NodeID != "node2" || got.Term != 7 {
		t.Errorf("status = %+v", got)
	}
}

func TestHTTPTransportRoundTrip(t *testing.T) {
	node := &fakeRaftNode{voteResp: raft.RequestVoteResponse{Term: 5, VoteGranted: true}}
	s := testServer(t, node, []string{"node1"})

	tr := NewHTTPTransport(map[string]string{"node2": s.URL}, testToken, nil)
	resp, err := tr.SendRequestVote(context.Background(), "node2", raft.RequestVoteRequest{
		ProtocolVersion: raft.ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node1", Term: 5, CandidateID: "node1",
	})
	if err != nil {
		t.Fatalf("SendRequestVote: %v", err)
	}
	if !resp.VoteGranted || resp.Term != 5 {
		t.Errorf("resp = %+v", resp)
	}

	// A bad token must surface as a transport error.
	badTr := NewHTTPTransport(map[string]string{"node2": s.URL}, "wrong", nil)
	if _, err := badTr.SendRequestVote(context.Background(), "node2", raft.RequestVoteRequest{
		ProtocolVersion: raft.ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node1",
	}); err == nil {
		t.Error("expected error with wrong cluster token")
	}
}

// TestHTTPTransportWithRealNode exercises the full HTTP path against a real Raft
// node: a higher-term RequestVote over HTTP must make the node step down.
func TestHTTPTransportWithRealNode(t *testing.T) {
	st, err := storage.Open(filepath.Join(t.TempDir(), "n", "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	node, err := raft.New(raft.Config{
		NodeID: "node2", ClusterID: "test-cluster", Peers: []string{"node1", "node2", "node3"},
	}, raft.Deps{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Stop)

	s := testServer(t, node, []string{"node1", "node3"})
	tr := NewHTTPTransport(map[string]string{"node2": s.URL}, testToken, nil)

	resp, err := tr.SendRequestVote(context.Background(), "node2", raft.RequestVoteRequest{
		ProtocolVersion: raft.ProtocolVersion, ClusterID: "test-cluster", SourceNodeID: "node1",
		Term: 9, CandidateID: "node1",
	})
	if err != nil {
		t.Fatalf("SendRequestVote: %v", err)
	}
	if resp.Term != 9 || !resp.VoteGranted {
		t.Errorf("resp term=%d granted=%v, want 9/true", resp.Term, resp.VoteGranted)
	}
	if term, _ := st.CurrentTerm(); term != 9 {
		t.Errorf("node did not persist term 9 (got %d) over HTTP", term)
	}
}

// ensure fakeRaftNode satisfies the interface at compile time.
var _ RaftNode = (*fakeRaftNode)(nil)
