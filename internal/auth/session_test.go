package auth

import (
	"errors"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	m := NewSessionManager([]byte("session-key-0123456789abcdef0123"), false)
	s, err := m.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if s.CSRFToken == "" {
		t.Error("session missing CSRF token")
	}

	got, err := m.Decode(m.Encode(s))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.CSRFToken != s.CSRFToken || got.ExpiresAtMS != s.ExpiresAtMS {
		t.Errorf("round trip mismatch: %+v vs %+v", got, s)
	}
}

func TestSessionTampered(t *testing.T) {
	m := NewSessionManager([]byte("session-key-0123456789abcdef0123"), false)
	s, _ := m.Issue()
	enc := m.Encode(s)

	// Flip a character in the payload.
	bad := "x" + enc[1:]
	if _, err := m.Decode(bad); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("tampered decode err = %v, want ErrInvalidSession", err)
	}
	if _, err := m.Decode("no-dot-here"); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("malformed decode err = %v, want ErrInvalidSession", err)
	}
}

func TestSessionWrongKey(t *testing.T) {
	a := NewSessionManager([]byte("key-aaaaaaaaaaaaaaaaaaaaaaaaaaaa"), false)
	b := NewSessionManager([]byte("key-bbbbbbbbbbbbbbbbbbbbbbbbbbbb"), false)
	s, _ := a.Issue()
	if _, err := b.Decode(a.Encode(s)); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("cross-key decode err = %v, want ErrInvalidSession", err)
	}
}

func TestSessionExpired(t *testing.T) {
	m := NewSessionManager([]byte("session-key-0123456789abcdef0123"), false)
	base := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return base }
	enc := m.Encode(mustIssue(t, m))

	// Advance beyond the TTL.
	m.now = func() time.Time { return base.Add(sessionTTL + time.Second) }
	if _, err := m.Decode(enc); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("expired decode err = %v, want ErrInvalidSession", err)
	}
}

func mustIssue(t *testing.T, m *SessionManager) Session {
	t.Helper()
	s, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
