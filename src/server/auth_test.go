package server

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHashTokenStable(t *testing.T) {
	// Pinned vector: sha256("hello") = 2cf24...
	got := HashToken("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("HashToken(\"hello\") = %q, want %q", got, want)
	}
}

func TestAuthorizeOK(t *testing.T) {
	a := NewAuthenticator(map[string]TokenEntry{
		HashToken("secret-1"): {Namespaces: []string{"alpha"}},
	})
	r := httptest.NewRequest("POST", "/v1/uploads/alpha", nil)
	r.Header.Set("Authorization", "Bearer secret-1")
	if err := a.Authorize(r, "alpha"); err != nil {
		t.Errorf("Authorize: %v", err)
	}
}

func TestAuthorizeWrongNamespace(t *testing.T) {
	a := NewAuthenticator(map[string]TokenEntry{
		HashToken("secret-1"): {Namespaces: []string{"alpha"}},
	})
	r := httptest.NewRequest("POST", "/v1/uploads/beta", nil)
	r.Header.Set("Authorization", "Bearer secret-1")
	if err := a.Authorize(r, "beta"); !errors.Is(err, ErrForbidden) {
		t.Errorf("Authorize wrong ns = %v, want ErrForbidden", err)
	}
}

func TestAuthorizeWildcard(t *testing.T) {
	a := NewAuthenticator(map[string]TokenEntry{
		HashToken("admin"): {Namespaces: []string{"*"}},
	})
	r := httptest.NewRequest("POST", "/v1/uploads/anything", nil)
	r.Header.Set("Authorization", "Bearer admin")
	if err := a.Authorize(r, "anything"); err != nil {
		t.Errorf("wildcard Authorize: %v", err)
	}
}

func TestAuthorizeMissingHeader(t *testing.T) {
	a := NewAuthenticator(nil)
	r := httptest.NewRequest("POST", "/v1/uploads/x", nil)
	if err := a.Authorize(r, "x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("missing header = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeMalformedHeader(t *testing.T) {
	a := NewAuthenticator(nil)
	r := httptest.NewRequest("POST", "/v1/uploads/x", nil)
	r.Header.Set("Authorization", "Token abc")
	if err := a.Authorize(r, "x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("malformed = %v, want ErrUnauthorized", err)
	}
}

func TestAuthorizeUnknownToken(t *testing.T) {
	a := NewAuthenticator(map[string]TokenEntry{
		HashToken("known"): {Namespaces: []string{"x"}},
	})
	r := httptest.NewRequest("POST", "/v1/uploads/x", nil)
	r.Header.Set("Authorization", "Bearer unknown")
	if err := a.Authorize(r, "x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unknown token = %v, want ErrUnauthorized", err)
	}
}

func TestLoadAuthenticator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	body := `{"tokens":{"` + HashToken("hello") + `":{"namespaces":["alpha","beta"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer hello")
	if err := a.Authorize(r, "alpha"); err != nil {
		t.Errorf("Authorize alpha: %v", err)
	}
	if err := a.Authorize(r, "gamma"); !errors.Is(err, ErrForbidden) {
		t.Errorf("Authorize gamma = %v, want ErrForbidden", err)
	}
}
