package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	requestIDHeader   = "X-Request-ID"
	idempotencyHeader = "Idempotency-Key"
	forwardedHeader   = "X-QL-Forwarded"
)

// requestID returns the caller-supplied request id if it is well-formed,
// otherwise a freshly generated one.
func requestID(r *http.Request) string {
	if id := r.Header.Get(requestIDHeader); validRequestID(id) {
		return id
	}
	return generateID("req_")
}

func validRequestID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == '~':
		default:
			return false
		}
	}
	return true
}

// generateID returns prefix + 24 random hex chars.
func generateID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// bearerToken extracts a bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(v, prefix))
	if tok == "" {
		return "", false
	}
	return tok, true
}
