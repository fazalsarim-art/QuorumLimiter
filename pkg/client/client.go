// Package client is a small, dependency-free Go client for the QuorumLimiter
// public decision API. It imports no internal packages so other projects can
// depend on it.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls a QuorumLimiter decision endpoint.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	maxRetries int
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithMaxRetries sets how many times a request carrying an idempotency key is
// retried on a retryable failure (503/504/network). Requests without an
// idempotency key are never retried.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// New creates a client for the given base URL and API key.
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		maxRetries: 2,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// DecideRequest is a decision request.
type DecideRequest struct {
	PolicyID string `json:"policy_id"`
	Subject  string `json:"subject"`
	Cost     int64  `json:"cost"`
}

// DecideResponse is a decision result. Allowed is true for 200 and false for
// 429; other status codes return an error instead.
type DecideResponse struct {
	StatusCode     int    `json:"-"`
	RequestID      string `json:"request_id"`
	PolicyID       string `json:"policy_id"`
	Allowed        bool   `json:"allowed"`
	Cost           int64  `json:"cost"`
	RemainingMilli int64  `json:"remaining_milli"`
	RetryAfterMS   int64  `json:"retry_after_ms"`
	ObservedAt     string `json:"observed_at"`
	LeaderTerm     uint64 `json:"leader_term"`
	LogIndex       uint64 `json:"log_index"`
	Duplicate      bool   `json:"duplicate"`
}

// APIError is a non-decision error response.
type APIError struct {
	StatusCode int
	Code       string `json:"code"`
	Message    string `json:"message"`
	RequestID  string `json:"request_id"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("quorumlimiter: %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// Decide asks whether the subject may proceed under the policy. idempotencyKey
// makes the call safe to retry: the same key returns the original decision
// without charging twice. When it is empty, the client does not retry (a retry
// could double-charge).
func (c *Client) Decide(ctx context.Context, req DecideRequest, idempotencyKey string) (*DecideResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	attempts := 1
	if idempotencyKey != "" {
		attempts += c.maxRetries
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		resp, err := c.do(ctx, body, idempotencyKey)
		if err != nil {
			lastErr = err
			if !idempotencyKey2Retryable(idempotencyKey) {
				return nil, err
			}
			continue
		}
		// Retry only 503/504 when an idempotency key is present.
		if (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) &&
			idempotencyKey != "" && i < attempts-1 {
			lastErr = decodeAPIError(resp)
			continue
		}
		return c.parse(resp)
	}
	return nil, lastErr
}

func (c *Client) do(ctx context.Context, body []byte, idempotencyKey string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return c.httpClient.Do(httpReq)
}

func (c *Client) parse(resp *http.Response) (*DecideResponse, error) {
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusTooManyRequests:
		var out DecideResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("quorumlimiter: decode response: %w", err)
		}
		out.StatusCode = resp.StatusCode
		return &out, nil
	default:
		return nil, apiErrorFromBody(resp.StatusCode, data)
	}
}

func decodeAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return apiErrorFromBody(resp.StatusCode, data)
}

func apiErrorFromBody(status int, data []byte) error {
	var env struct {
		Error APIError `json:"error"`
	}
	_ = json.Unmarshal(data, &env)
	env.Error.StatusCode = status
	return &env.Error
}

// idempotencyKey2Retryable reports whether a network error is retryable, which
// is only when an idempotency key was supplied.
func idempotencyKey2Retryable(idempotencyKey string) bool { return idempotencyKey != "" }
