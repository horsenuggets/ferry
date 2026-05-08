package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	c := &Config{}
	c.ApplyDefaults()
	if c.ListenAddr != "0.0.0.0:7421" {
		t.Errorf("default ListenAddr = %q, want 0.0.0.0:7421", c.ListenAddr)
	}
	if c.DataDir != "/var/lib/ferry/data" {
		t.Errorf("default DataDir = %q", c.DataDir)
	}
	if c.MaxPatchBytes != 64*1024*1024 {
		t.Errorf("default MaxPatchBytes = %d, want %d", c.MaxPatchBytes, 64*1024*1024)
	}
	if c.DiskSafetyMarginBytes != 1<<30 {
		t.Errorf("default DiskSafetyMarginBytes = %d, want %d", c.DiskSafetyMarginBytes, 1<<30)
	}
}

func TestApplyDefaultsKeepsOverrides(t *testing.T) {
	c := &Config{ListenAddr: "127.0.0.1:9000", MaxPatchBytes: 5}
	c.ApplyDefaults()
	if c.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("ListenAddr override lost: %q", c.ListenAddr)
	}
	if c.MaxPatchBytes != 5 {
		t.Errorf("MaxPatchBytes override lost: %d", c.MaxPatchBytes)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"listen_addr":"127.0.0.1:7421","data_dir":"` + dir + `/data","tokens_path":"` + dir + `/tokens.json"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:7421" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.MaxPatchBytes == 0 {
		t.Errorf("MaxPatchBytes default not applied")
	}
}

func TestLoadConfigMissing(t *testing.T) {
	if _, err := LoadConfig("/nonexistent/ferry.json"); err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestLoadConfigBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for bad json")
	}
}
