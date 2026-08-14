package main

import "testing"

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

	on := true
	cfg.BlockRetry = &on
	if !cfg.blockRetryEnabled() {
		t.Error("block retry should be enabled when block_retry=true")
	}
}
