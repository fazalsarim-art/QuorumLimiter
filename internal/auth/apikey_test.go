package auth

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/limiter"
)

type fakeClients struct {
	byPrefix map[string]string
	byID     map[string][]byte
}

func (f *fakeClients) ClientIDByPrefix(prefix string) (string, bool, error) {
	id, ok := f.byPrefix[prefix]
	return id, ok, nil
}

func (f *fakeClients) Client(id string) ([]byte, bool, error) {
	b, ok := f.byID[id]
	return b, ok, nil
}

func TestAuthenticate(t *testing.T) {
	pepper := []byte("test-pepper-0123456789abcdef0123")
	rawKey := "qlk_ABCDEFGH_secretvalue1234567890"

	// Build a client whose digest matches the raw key.
	digest := NewAPIKeyAuthenticator(pepper, nil).Digest(rawKey)
	client := limiter.Client{
		ID: "cli_1", Name: "Test", KeyPrefix: "ABCDEFGH", KeyDigest: digest,
		AllowedPolicyIDs: []string{"*"}, Active: true,
	}
	body, _ := json.Marshal(client)
	fc := &fakeClients{
		byPrefix: map[string]string{"ABCDEFGH": "cli_1"},
		byID:     map[string][]byte{"cli_1": body},
	}
	a := NewAPIKeyAuthenticator(pepper, fc)

	t.Run("valid key", func(t *testing.T) {
		got, err := a.Authenticate(rawKey)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if got.ID != "cli_1" {
			t.Errorf("client = %q, want cli_1", got.ID)
		}
	})

	t.Run("wrong secret same prefix", func(t *testing.T) {
		if _, err := a.Authenticate("qlk_ABCDEFGH_wrongsecret0000000000"); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("err = %v, want ErrInvalidAPIKey", err)
		}
	})

	t.Run("unknown prefix", func(t *testing.T) {
		if _, err := a.Authenticate("qlk_ZZZZZZZZ_secretvalue1234567890"); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("err = %v, want ErrInvalidAPIKey", err)
		}
	})

	t.Run("malformed key", func(t *testing.T) {
		for _, bad := range []string{"", "notakey", "qlk_short", "qlk_ABCDEFGH_", "ABCDEFGH_secret"} {
			if _, err := a.Authenticate(bad); !errors.Is(err, ErrInvalidAPIKey) {
				t.Errorf("Authenticate(%q) err = %v, want ErrInvalidAPIKey", bad, err)
			}
		}
	})

	t.Run("revoked client", func(t *testing.T) {
		revoked := client
		revoked.Active = false
		rb, _ := json.Marshal(revoked)
		fc.byID["cli_1"] = rb
		defer func() { fc.byID["cli_1"] = body }()
		if _, err := a.Authenticate(rawKey); !errors.Is(err, ErrInvalidAPIKey) {
			t.Errorf("revoked err = %v, want ErrInvalidAPIKey", err)
		}
	})
}
