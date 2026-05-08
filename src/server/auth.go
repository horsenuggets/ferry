package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// TokenEntry describes a single bearer token's permissions. The map key in
// TokensFile is the SHA-256 hex digest of the bearer token; we never persist
// or load the plaintext token on the server.
type TokenEntry struct {
	Namespaces []string `json:"namespaces"`
}

// TokensFile is the on-disk schema for tokens.json.
type TokensFile struct {
	Tokens map[string]TokenEntry `json:"tokens"`
}

// Authenticator authorizes Bearer tokens against a static set of token hashes
// and namespace allowlists. The wildcard "*" in a token's namespaces grants
// access to all namespaces.
type Authenticator struct {
	tokens map[string]TokenEntry // key = sha256-hex of token
}

// LoadAuthenticator parses a tokens file from path.
func LoadAuthenticator(path string) (*Authenticator, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	var tf TokensFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("parse tokens: %w", err)
	}
	if tf.Tokens == nil {
		tf.Tokens = map[string]TokenEntry{}
	}
	return &Authenticator{tokens: tf.Tokens}, nil
}

// NewAuthenticator builds an Authenticator from an in-memory map of token
// hash -> entry. Useful for tests.
func NewAuthenticator(tokens map[string]TokenEntry) *Authenticator {
	if tokens == nil {
		tokens = map[string]TokenEntry{}
	}
	return &Authenticator{tokens: tokens}
}

// HashToken computes the SHA-256 hex digest of a plaintext bearer token.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Authorize returns nil if the request carries a valid Bearer token that is
// scoped to namespace. Returns ErrUnauthorized if no/malformed token, or
// ErrForbidden if the token is valid but does not authorize this namespace.
func (a *Authenticator) Authorize(r *http.Request, namespace string) error {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ErrUnauthorized
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ErrUnauthorized
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return ErrUnauthorized
	}
	entry, ok := a.tokens[HashToken(token)]
	if !ok {
		return ErrUnauthorized
	}
	for _, ns := range entry.Namespaces {
		if ns == "*" || ns == namespace {
			return nil
		}
	}
	return ErrForbidden
}
