package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

const (
	defaultListen   = ":8080"
	defaultUpstream = "https://generativelanguage.googleapis.com"
	defaultMaxRetry = 5
	defaultRPMLock  = 60 // 非 RPD 的 429 固定冷却秒数
)

type Config struct {
	Listen        string   `json:"listen"`
	Upstream      string   `json:"upstream"`
	MaxRetries    int      `json:"max_retries"`
	Keys          []string `json:"keys"`
	AdminPassword string   `json:"admin_password"` // WebUI/管理 API 的 Basic Auth 密码，留空则无认证
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.Upstream == "" {
		c.Upstream = defaultUpstream
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = defaultMaxRetry
	}
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("config %s: keys must not be empty", path)
	}
	for i, k := range cfg.Keys {
		if k == "" {
			return nil, fmt.Errorf("config %s: key[%d] is empty", path, i)
		}
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// generatePassword 生成 24 位十六进制随机密码。
func generatePassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// saveConfigPassword 将生成的密码写回配置文件（保留其余字段）。
func saveConfigPassword(path, password string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["admin_password"] = password
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}
