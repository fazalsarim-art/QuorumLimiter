package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fazalsarim-art/QuorumLimiter/internal/raft"
)

// maxInternalBody bounds internal RPC request bodies.
const maxInternalBody = 64 << 10 // 64 KiB

// RaftNode is the subset of *raft.Node the internal endpoints need.
type RaftNode interface {
	HandleRequestVote(ctx context.Context, req raft.RequestVoteRequest) (raft.RequestVoteResponse, error)
	HandleAppendEntries(ctx context.Context, req raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error)
	Status() (raft.Status, error)
}

// InternalRaftHandler serves the private /internal/raft/* endpoints. Every
// request must present the cluster bearer token and, for RPCs, name an allowed
// source node.
type InternalRaftHandler struct {
	node         RaftNode
	clusterToken string
	allowed      map[string]bool
	log          *slog.Logger
}

// NewInternalRaftHandler builds a handler. allowedSources is the set of peer node
// IDs permitted to call the RPC endpoints (typically the other two nodes).
func NewInternalRaftHandler(node RaftNode, clusterToken string, allowedSources []string, logger *slog.Logger) *InternalRaftHandler {
	if logger == nil {
		logger = slog.Default()
	}
	allowed := make(map[string]bool, len(allowedSources))
	for _, s := range allowedSources {
		allowed[s] = true
	}
	return &InternalRaftHandler{
		node:         node,
		clusterToken: clusterToken,
		allowed:      allowed,
		log:          logger.With(slog.String("component", "internal-raft")),
	}
}

func (h *InternalRaftHandler) handleRequestVote(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req raft.RequestVoteRequest
	if err := decodeInternal(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if !h.allowed[req.SourceNodeID] {
		writeError(w, http.StatusForbidden, "source_not_allowed")
		return
	}
	resp, err := h.node.HandleRequestVote(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *InternalRaftHandler) handleAppendEntries(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req raft.AppendEntriesRequest
	if err := decodeInternal(w, r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if !h.allowed[req.SourceNodeID] {
		writeError(w, http.StatusForbidden, "source_not_allowed")
		return
	}
	resp, err := h.node.HandleAppendEntries(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *InternalRaftHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s, err := h.node.Status()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// authorized checks the cluster bearer token in constant time.
func (h *InternalRaftHandler) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	token := strings.TrimPrefix(got, prefix)
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.clusterToken)) == 1
}

var errUnsupportedMedia = errors.New("unsupported media type")

// decodeInternal enforces JSON content type, a body-size cap, and strict
// decoding (unknown fields rejected).
func decodeInternal(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errUnsupportedMedia
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInternalBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing data after the first JSON value.
	if dec.More() {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, errUnsupportedMedia):
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
	case errors.As(err, &maxErr):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large")
	default:
		writeError(w, http.StatusBadRequest, "invalid_request")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

// --- HTTP transport (client side) ---

// HTTPTransport implements raft.Transport over HTTP, sending internal RPCs to
// peers identified by node ID.
type HTTPTransport struct {
	peerURLs     map[string]string // node ID -> base URL (no trailing slash)
	clusterToken string
	client       *http.Client
}

// NewHTTPTransport builds an HTTP transport. peerURLs maps peer node IDs to their
// base URLs.
func NewHTTPTransport(peerURLs map[string]string, clusterToken string, client *http.Client) *HTTPTransport {
	if client == nil {
		client = &http.Client{}
	}
	urls := make(map[string]string, len(peerURLs))
	for id, u := range peerURLs {
		urls[id] = strings.TrimRight(u, "/")
	}
	return &HTTPTransport{peerURLs: urls, clusterToken: clusterToken, client: client}
}

// SendRequestVote implements raft.Transport.
func (t *HTTPTransport) SendRequestVote(ctx context.Context, target string, req raft.RequestVoteRequest) (raft.RequestVoteResponse, error) {
	var resp raft.RequestVoteResponse
	err := t.post(ctx, target, "/internal/raft/request-vote", req, &resp)
	return resp, err
}

// SendAppendEntries implements raft.Transport.
func (t *HTTPTransport) SendAppendEntries(ctx context.Context, target string, req raft.AppendEntriesRequest) (raft.AppendEntriesResponse, error) {
	var resp raft.AppendEntriesResponse
	err := t.post(ctx, target, "/internal/raft/append-entries", req, &resp)
	return resp, err
}

func (t *HTTPTransport) post(ctx context.Context, target, path string, req, respDst any) error {
	base, ok := t.peerURLs[target]
	if !ok {
		return fmt.Errorf("raft transport: unknown peer %q", target)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("raft transport: encode: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("raft transport: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.clusterToken)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("raft transport: %s: %w", target, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("raft transport: %s returned status %d", target, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(respDst); err != nil {
		return fmt.Errorf("raft transport: decode response: %w", err)
	}
	return nil
}
