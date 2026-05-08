// Package client implements the ferry uploader: a tus-1.0.0 compatible
// resumable upload client with bearer auth, namespace scoping, idempotency
// support, and per-chunk retry with HEAD-based offset recovery.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultChunkSizeBytes is the default PATCH body size when neither the CLI
// nor the on-disk config overrides it. 4 MiB is a good compromise between
// throughput and the cost of re-sending a chunk on a retry.
const DefaultChunkSizeBytes = 4 * 1024 * 1024

// MaxChunkSizeBytes mirrors the server's max_patch_bytes hard cap. Chunks
// larger than this would be rejected with 413, so the client refuses to send
// them in the first place.
const MaxChunkSizeBytes = 64 * 1024 * 1024

// Config is the on-disk client config (typically ~/.config/ferry/config.json).
// All fields are optional; CLI flags override these values.
type Config struct {
	DefaultURL            string `json:"default_url,omitempty"`
	DefaultNamespace      string `json:"default_namespace,omitempty"`
	DefaultToken          string `json:"default_token,omitempty"`
	DefaultChunkSizeBytes int64  `json:"default_chunk_size_bytes,omitempty"`
}

// DefaultConfigPath returns the conventional config location, expanding the
// user's home directory if available. Falls back to a relative path if HOME
// cannot be resolved.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "ferry", "config.json")
	}
	return filepath.Join(home, ".config", "ferry", "config.json")
}

// LoadConfig reads and parses a client config from path. Missing files are
// returned as a zero-value Config with no error, so callers can layer flags
// and env vars on top without special-casing "no config yet".
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return c, nil
}

// Resolved is the set of values used by Upload/Status after layering CLI
// flags > env vars > config file. All required fields are guaranteed
// non-empty / positive.
type Resolved struct {
	URL       string
	Namespace string
	Token     string
	ChunkSize int64
}

// ResolveInput captures CLI flag values and the loaded Config.
//
// The resolution order is: explicit CLI flag > matching env var > config
// file value > error if a required field remains empty.
//
// chunkSize is clamped to [1, MaxChunkSizeBytes]. Zero is treated as
// "not provided" at every layer.
type ResolveInput struct {
	FlagURL       string
	FlagNamespace string
	FlagToken     string
	FlagChunkSize int64
	Config        Config
	// Env is the environment lookup function. Tests inject a fake; real
	// callers pass os.Getenv.
	Env func(string) string
}

// Resolve layers the inputs and returns either a fully-resolved struct or
// an error naming the first missing required field.
func Resolve(in ResolveInput) (Resolved, error) {
	getenv := in.Env
	if getenv == nil {
		getenv = os.Getenv
	}
	pick := func(flag, env, cfg string) string {
		if flag != "" {
			return flag
		}
		if v := getenv(env); v != "" {
			return v
		}
		return cfg
	}
	url := pick(in.FlagURL, "FERRY_URL", in.Config.DefaultURL)
	ns := pick(in.FlagNamespace, "FERRY_NAMESPACE", in.Config.DefaultNamespace)
	tok := pick(in.FlagToken, "FERRY_TOKEN", in.Config.DefaultToken)

	if url == "" {
		return Resolved{}, errors.New("missing peer URL: pass --to or set FERRY_URL or default_url")
	}
	if ns == "" {
		return Resolved{}, errors.New("missing namespace: pass --namespace or set FERRY_NAMESPACE or default_namespace")
	}
	if tok == "" {
		return Resolved{}, errors.New("missing token: pass --token or set FERRY_TOKEN or default_token")
	}

	chunk := in.FlagChunkSize
	if chunk == 0 {
		if v := getenv("FERRY_CHUNK_SIZE_BYTES"); v != "" {
			// Ignore parse errors; treat as absent to fall through to config/default.
			if parsed, err := parsePositiveInt64(v); err == nil {
				chunk = parsed
			}
		}
	}
	if chunk == 0 {
		chunk = in.Config.DefaultChunkSizeBytes
	}
	if chunk == 0 {
		chunk = DefaultChunkSizeBytes
	}
	if chunk < 1 {
		chunk = DefaultChunkSizeBytes
	}
	if chunk > MaxChunkSizeBytes {
		chunk = MaxChunkSizeBytes
	}
	return Resolved{
		URL:       url,
		Namespace: ns,
		Token:     tok,
		ChunkSize: chunk,
	}, nil
}

func parsePositiveInt64(s string) (int64, error) {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int64(r-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("not positive: %q", s)
	}
	return n, nil
}
