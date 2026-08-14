package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

const (
	defaultListen          = ":8080"
	defaultUpstream        = "https://generativelanguage.googleapis.com"
	defaultMaxRetry        = 5
	defaultRequestTimeout  = 30 // 上游响应头等待超时秒数
	defaultRPMLock         = 60 // 非 RPD 的 429 固定冷却秒数
	defaultMaxBlockRetries = 1  // 安全拦截自动重试次数上限
)

type Config struct {
	Listen          string   `json:"listen"`
	Upstream        string   `json:"upstream"`
	MaxRetries      int      `json:"max_retries"`
	RequestTimeout  int      `json:"request_timeout"`   // 秒；上游未在超时内发出响应头则网关直接回 503
	BlockRetry      *bool    `json:"block_retry"`       // 安全拦截自动重试；省略字段时默认开启
	MaxBlockRetries int      `json:"max_block_retries"` // 安全拦截自动重试次数上限（默认 1）
	Keys            []string `json:"keys"`
	AdminPassword   string   `json:"admin_password"` // WebUI/管理 API 的 Basic Auth 密码，留空则无认证
}

// blockRetryEnabled 返回安全拦截自动重试开关；省略 block_retry 字段时默认开启。
func (c *Config) blockRetryEnabled() bool {
	return c.BlockRetry == nil || *c.BlockRetry
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
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.MaxBlockRetries <= 0 {
		c.MaxBlockRetries = defaultMaxBlockRetries
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
	return updateConfig(path, func(m map[string]any) {
		m["admin_password"] = password
	})
}

// saveConfigKeys 将 Key 列表写回配置文件（保留其余字段，如 admin_password）。
func saveConfigKeys(path string, keys []string) error {
	return updateConfig(path, func(m map[string]any) {
		m["keys"] = keys
	})
}

// updateConfig 读取配置文件、调用 mutate 修改字段后原子写回。
func updateConfig(path string, mutate func(map[string]any)) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	mutate(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}
