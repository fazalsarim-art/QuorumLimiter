package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
)

// base32NoPad encodes fixed-length, uppercase, unpadded identifiers.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// API key format: qlk_<8-char prefix>_<secret>. The prefix is used for lookup;
// the whole key is HMAC'd (with the pepper) and compared in constant time.
const (
	keyScheme    = "qlk"
	keyPrefixLen = 8
)

// ErrInvalidAPIKey is returned for any authentication failure. It is
// deliberately generic so callers cannot distinguish unknown, malformed,
// mismatched, or revoked keys (avoiding an oracle).
var ErrInvalidAPIKey = errors.New("auth: invalid api key")

// ClientReader reads client records for authentication. *storage.Store satisfies
// it.
type ClientReader interface {
	ClientIDByPrefix(prefix string) (string, bool, error)
	Client(id string) ([]byte, bool, error)
}

// APIKeyAuthenticator verifies client API keys against stored prefixes and
// HMAC digests.
type APIKeyAuthenticator struct {
	pepper  []byte
	clients ClientReader
}

// NewAPIKeyAuthenticator builds an authenticator using the given pepper (HMAC
// key) and client store.
func NewAPIKeyAuthenticator(pepper []byte, clients ClientReader) *APIKeyAuthenticator {
	return &APIKeyAuthenticator{pepper: pepper, clients: clients}
}

// Authenticate verifies a raw API key and returns the owning client. It always
// returns ErrInvalidAPIKey on any failure, including a revoked client.
func (a *APIKeyAuthenticator) Authenticate(rawKey string) (limiter.Client, error) {
	prefix, err := parseKeyPrefix(rawKey)
	if err != nil {
		return limiter.Client{}, ErrInvalidAPIKey
	}
	clientID, ok, err := a.clients.ClientIDByPrefix(prefix)
	if err != nil {
		return limiter.Client{}, err
	}
	if !ok {
		return limiter.Client{}, ErrInvalidAPIKey
	}
	raw, ok, err := a.clients.Client(clientID)
	if err != nil {
		return limiter.Client{}, err
	}
	if !ok {
		return limiter.Client{}, ErrInvalidAPIKey
	}
	client, err := limiter.DecodeClient(raw)
	if err != nil {
		return limiter.Client{}, err
	}
	if subtle.ConstantTimeCompare(client.KeyDigest, a.Digest(rawKey)) != 1 {
		return limiter.Client{}, ErrInvalidAPIKey
	}
	if !client.Active {
		return limiter.Client{}, ErrInvalidAPIKey
	}
	return client, nil
}

// Digest computes the HMAC-SHA256 of a raw key with the pepper. It is also used
// when creating clients to derive the stored digest.
func (a *APIKeyAuthenticator) Digest(rawKey string) []byte {
	m := hmac.New(sha256.New, a.pepper)
	m.Write([]byte(rawKey))
	return m.Sum(nil)
}

// GenerateAPIKey creates a fresh raw API key (qlk_<8-char prefix>_<secret>) and
// returns it with its prefix. The raw key is shown to the operator once and is
// never stored; only the prefix and an HMAC digest are persisted.
func GenerateAPIKey() (rawKey, prefix string, err error) {
	pb := make([]byte, 5) // 5 bytes -> exactly 8 base32 chars
	if _, err = rand.Read(pb); err != nil {
		return "", "", err
	}
	prefix = base32NoPad.EncodeToString(pb)
	sb := make([]byte, 32)
	if _, err = rand.Read(sb); err != nil {
		return "", "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(sb)
	return keyScheme + "_" + prefix + "_" + secret, prefix, nil
}

// GenerateClientID creates a server-side client identifier (cli_<base32>).
func GenerateClientID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cli_" + base32NoPad.EncodeToString(b), nil
}

// parseKeyPrefix extracts the lookup prefix from a raw key without revealing why
// a malformed key failed.
func parseKeyPrefix(rawKey string) (string, error) {
	rest, ok := strings.CutPrefix(rawKey, keyScheme+"_")
	if !ok {
		return "", ErrInvalidAPIKey
	}
	if len(rest) < keyPrefixLen+1 || rest[keyPrefixLen] != '_' {
		return "", ErrInvalidAPIKey
	}
	prefix := rest[:keyPrefixLen]
	secret := rest[keyPrefixLen+1:]
	if prefix == "" || secret == "" {
		return "", ErrInvalidAPIKey
	}
	return prefix, nil
}
