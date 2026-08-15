package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlockRetryDefaultsOff(t *testing.T) {
	var cfg Config
	if cfg.blockRetryEnabled() {
		t.Error("block retry should be disabled by default")
	}
	cfg.applyDefaults()
	if cfg.blockRetryEnabled() {
		t.Error("block retry should stay disabled after applyDefaults")
	}
	if cfg.MaxBlockRetries != 0 {
		t.Errorf("MaxBlockRetries default = %d, want 0", cfg.MaxBlockRetries)
	}
	if cfg.BlockRetryMode != BlockRetryModeFull {
		t.Errorf("BlockRetryMode default = %q, want %q", cfg.BlockRetryMode, BlockRetryModeFull)
	}

	on := true
	cfg.BlockRetry = &on
	if !cfg.blockRetryEnabled() {
		t.Error("block retry should be enabled when block_retry=true")
	}
}

func TestLoadConfigInvalidBlockRetryMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"keys":["k"],"block_retry_mode":"weird"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig should reject invalid block_retry_mode")
	}
	if !strings.Contains(err.Error(), "block_retry_mode") {
		t.Errorf("error = %q, want mention block_retry_mode", err)
	}
}
