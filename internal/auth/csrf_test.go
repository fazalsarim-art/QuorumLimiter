package auth

import (
	"net/http"
	"testing"
)

func TestVerifyCSRFToken(t *testing.T) {
	s := Session{CSRFToken: "correct-token"}
	if !VerifyCSRFToken(s, "correct-token") {
		t.Error("matching token should verify")
	}
	if VerifyCSRFToken(s, "wrong") {
		t.Error("wrong token should not verify")
	}
	if VerifyCSRFToken(s, "") {
		t.Error("empty token should not verify")
	}
	if VerifyCSRFToken(Session{}, "anything") {
		t.Error("empty session token should not verify")
	}
}

func TestSameOrigin(t *testing.T) {
	newReq := func(origin, referer string) *http.Request {
		r := &http.Request{Host: "limiter.example.com", Header: http.Header{}}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}

	if !SameOrigin(newReq("https://limiter.example.com", "")) {
		t.Error("matching Origin should pass")
	}
	if SameOrigin(newReq("https://evil.example.com", "")) {
		t.Error("mismatched Origin should fail")
	}
	if !SameOrigin(newReq("", "https://limiter.example.com/admin")) {
		t.Error("matching Referer should pass")
	}
	if SameOrigin(newReq("", "")) {
		t.Error("missing Origin and Referer should fail (strict)")
	}
}
