package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_MissingFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatalf("expected nil err on missing file, got %v", err)
	}
	if (cfg != Config{}) {
		t.Fatalf("expected zero Config, got %+v", cfg)
	}
}

func TestLoadConfig_ParsesValidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	contents := `{
		"default_url": "http://h:1",
		"default_namespace": "ns",
		"default_token": "tk",
		"default_chunk_size_bytes": 1048576
	}`
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultURL != "http://h:1" || cfg.DefaultNamespace != "ns" ||
		cfg.DefaultToken != "tk" || cfg.DefaultChunkSizeBytes != 1048576 {
		t.Fatalf("unexpected: %+v", cfg)
	}
}

func TestLoadConfig_BadJSONErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestResolve_FlagBeatsEnvBeatsConfig(t *testing.T) {
	cfg := Config{
		DefaultURL:            "from-config",
		DefaultNamespace:      "ns-config",
		DefaultToken:          "tk-config",
		DefaultChunkSizeBytes: 1234,
	}
	env := map[string]string{
		"FERRY_URL":              "from-env",
		"FERRY_NAMESPACE":        "ns-env",
		"FERRY_TOKEN":            "tk-env",
		"FERRY_CHUNK_SIZE_BYTES": "5678",
	}
	getenv := func(k string) string { return env[k] }

	// Config-only.
	r, err := Resolve(ResolveInput{Config: cfg, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "from-config" || r.Namespace != "ns-config" || r.Token != "tk-config" || r.ChunkSize != 1234 {
		t.Fatalf("config-only: %+v", r)
	}

	// Env > config.
	r, err = Resolve(ResolveInput{Config: cfg, Env: getenv})
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "from-env" || r.Namespace != "ns-env" || r.Token != "tk-env" || r.ChunkSize != 5678 {
		t.Fatalf("env > config: %+v", r)
	}

	// Flag > env > config.
	r, err = Resolve(ResolveInput{
		FlagURL:       "flag-url",
		FlagNamespace: "flag-ns",
		FlagToken:     "flag-tk",
		FlagChunkSize: 9999,
		Config:        cfg,
		Env:           getenv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "flag-url" || r.Namespace != "flag-ns" || r.Token != "flag-tk" || r.ChunkSize != 9999 {
		t.Fatalf("flag wins: %+v", r)
	}
}

func TestResolve_MissingFieldsError(t *testing.T) {
	noEnv := func(string) string { return "" }
	if _, err := Resolve(ResolveInput{Env: noEnv}); err == nil {
		t.Fatal("expected error: no URL")
	}
	if _, err := Resolve(ResolveInput{FlagURL: "x", Env: noEnv}); err == nil {
		t.Fatal("expected error: no namespace")
	}
	if _, err := Resolve(ResolveInput{FlagURL: "x", FlagNamespace: "y", Env: noEnv}); err == nil {
		t.Fatal("expected error: no token")
	}
	if _, err := Resolve(ResolveInput{FlagURL: "x", FlagNamespace: "y", FlagToken: "z", Env: noEnv}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolve_ChunkSizeDefaultsAndCap(t *testing.T) {
	noEnv := func(string) string { return "" }
	r, err := Resolve(ResolveInput{FlagURL: "x", FlagNamespace: "y", FlagToken: "z", Env: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if r.ChunkSize != DefaultChunkSizeBytes {
		t.Fatalf("expected default chunk size, got %d", r.ChunkSize)
	}

	// Over the cap clamps down.
	r, err = Resolve(ResolveInput{
		FlagURL: "x", FlagNamespace: "y", FlagToken: "z",
		FlagChunkSize: MaxChunkSizeBytes * 2,
		Env:           noEnv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ChunkSize != MaxChunkSizeBytes {
		t.Fatalf("expected clamp to MaxChunkSizeBytes, got %d", r.ChunkSize)
	}
}

func TestDefaultConfigPath_NonEmpty(t *testing.T) {
	if DefaultConfigPath() == "" {
		t.Fatal("DefaultConfigPath returned empty")
	}
}
