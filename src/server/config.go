package server

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the JSON-serialized server config (typically /etc/ferry/config.json).
// Tokens live in a separate file (TokensPath) so the main config can be
// world-readable while tokens stay 0600.
type Config struct {
	ListenAddr                 string `json:"listen_addr"`
	DataDir                    string `json:"data_dir"`
	TokensPath                 string `json:"tokens_path"`
	CompletedRetentionSeconds  int64  `json:"completed_retention_seconds"`
	IncompleteRetentionSeconds int64  `json:"incomplete_retention_seconds"`
	GCIntervalSeconds          int64  `json:"gc_interval_seconds"`
	MaxPatchBytes              int64  `json:"max_patch_bytes"`
	DiskSafetyMarginBytes      int64  `json:"disk_safety_margin_bytes"`
}

// Defaults returns a Config with sensible defaults applied. Zero/empty fields
// are filled in; non-zero fields are left as-is.
func (c *Config) ApplyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = "0.0.0.0:7421"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/ferry/data"
	}
	if c.TokensPath == "" {
		c.TokensPath = "/etc/ferry/tokens.json"
	}
	if c.CompletedRetentionSeconds == 0 {
		c.CompletedRetentionSeconds = 86400 // 24h
	}
	if c.IncompleteRetentionSeconds == 0 {
		c.IncompleteRetentionSeconds = 604800 // 7d
	}
	if c.GCIntervalSeconds == 0 {
		c.GCIntervalSeconds = 3600 // 1h
	}
	if c.MaxPatchBytes == 0 {
		c.MaxPatchBytes = 64 * 1024 * 1024 // 64 MiB
	}
	if c.DiskSafetyMarginBytes == 0 {
		c.DiskSafetyMarginBytes = 1 << 30 // 1 GiB
	}
}

// LoadConfig reads a JSON config from path and applies defaults.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.ApplyDefaults()
	return &c, nil
}
