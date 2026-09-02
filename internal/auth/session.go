package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Cookie names and default session lifetime.
const (
	SessionCookieName = "ql_admin_session"
	CSRFCookieName    = "ql_csrf"
	sessionTTL        = 8 * time.Hour
)

// ErrInvalidSession is returned for a missing, malformed, tampered, or expired
// session.
var ErrInvalidSession = errors.New("auth: invalid session")

// Session is the signed admin session payload.
type Session struct {
	IssuedAtMS  int64  `json:"iat"`
	ExpiresAtMS int64  `json:"exp"`
	CSRFToken   string `json:"csrf"`
}

// SessionManager issues and verifies signed session cookies using the shared
// session key (so any node validates any node's cookie).
type SessionManager struct {
	key        []byte
	production bool
	ttl        time.Duration
	now        func() time.Time
}

// NewSessionManager builds a manager. production enables the Secure cookie flag.
func NewSessionManager(key []byte, production bool) *SessionManager {
	return &SessionManager{key: key, production: production, ttl: sessionTTL, now: time.Now}
}

// Issue creates a new session with a fresh random CSRF token.
func (m *SessionManager) Issue() (Session, error) {
	csrf, err := randomToken(24)
	if err != nil {
		return Session{}, err
	}
	now := m.now()
	return Session{
		IssuedAtMS:  now.UnixMilli(),
		ExpiresAtMS: now.Add(m.ttl).UnixMilli(),
		CSRFToken:   csrf,
	}, nil
}

// Encode serializes and signs a session into a cookie value (payload.mac).
func (m *SessionManager) Encode(s Session) string {
	payload, _ := json.Marshal(s)
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := m.sign([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac)
}

// Decode verifies the signature and expiry and returns the session.
func (m *SessionManager) Decode(value string) (Session, error) {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return Session{}, ErrInvalidSession
	}
	p, sig := value[:dot], value[dot+1:]
	gotMac, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(m.sign([]byte(p)), gotMac) != 1 {
		return Session{}, ErrInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}, ErrInvalidSession
	}
	if m.now().UnixMilli() >= s.ExpiresAtMS {
		return Session{}, ErrInvalidSession
	}
	return s, nil
}

func (m *SessionManager) sign(b []byte) []byte {
	h := hmac.New(sha256.New, m.key)
	h.Write(b)
	return h.Sum(nil)
}

// FromRequest reads and verifies the session cookie on a request.
func (m *SessionManager) FromRequest(r *http.Request) (Session, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	return m.Decode(c.Value)
}

// SessionCookie builds the signed, HttpOnly session cookie.
func (m *SessionManager) SessionCookie(s Session) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    m.Encode(s),
		Path:     "/",
		HttpOnly: true,
		Secure:   m.production,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.UnixMilli(s.ExpiresAtMS),
		MaxAge:   int(m.ttl.Seconds()),
	}
}

// CSRFCookie builds the readable companion cookie carrying the CSRF token, so a
// client can echo it in the X-CSRF-Token header (double-submit). The
// authoritative value lives in the signed session.
func (m *SessionManager) CSRFCookie(s Session) *http.Cookie {
	return &http.Cookie{
		Name:     CSRFCookieName,
		Value:    s.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   m.production,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(m.ttl.Seconds()),
	}
}

// ClearCookies returns expired cookies that remove the session and CSRF cookies.
func (m *SessionManager) ClearCookies() []*http.Cookie {
	return []*http.Cookie{
		{Name: SessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: m.production, SameSite: http.SameSiteStrictMode, MaxAge: -1},
		{Name: CSRFCookieName, Value: "", Path: "/", Secure: m.production, SameSite: http.SameSiteStrictMode, MaxAge: -1},
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
