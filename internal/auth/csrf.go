package auth

import (
	"crypto/subtle"
	"net/http"
	"net/url"
)

// VerifyCSRFToken reports whether the provided token matches the session's CSRF
// token, in constant time.
func VerifyCSRFToken(session Session, provided string) bool {
	if provided == "" || session.CSRFToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(provided)) == 1
}

// SameOrigin checks that a state-changing request originated from the same site.
// It compares the Origin (or, failing that, the Referer) host to the request's
// own host. A mutation with neither header is rejected.
func SameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && u.Host != "" && u.Host == r.Host
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Host != "" && u.Host == r.Host
	}
	return false
}
