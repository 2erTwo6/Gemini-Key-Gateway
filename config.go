package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// configFileMu 串行化所有对 config.json 的读-改-写，避免 WebUI 并发操作
// （Key 增删、设置保存、密码写回）相互覆盖。
var configFileMu sync.Mutex

const (
	defaultListen          = ":8080"
	defaultUpstream        = "https://generativelanguage.googleapis.com"
	defaultMaxRetry        = 5
	defaultRequestTimeout  = 30 // 上游响应头等待超时秒数
	defaultRPMLock         = 60 // 非 RPD 的 429 固定冷却秒数
	defaultMaxBlockRetries = 0  // 安全拦截自动重试次数上限（0 = 默认关闭）

	// BlockRetryModeFull 完整缓冲 content 端点 2xx 响应后判定拦截（默认，兼容旧行为）。
	BlockRetryModeFull = "full"
	// BlockRetryModeStream 只检查流式响应首块（SSE 首事件 / JSON 数组首元素），
	// 未拦截时立即透传，保持流式实时性；流中途被安全截断不再重试。
	BlockRetryModeStream = "stream"
)

type Config struct {
	Listen          string   `json:"listen"`
	Upstream        string   `json:"upstream"`
	MaxRetries      *int     `json:"max_retries"`       // 一次请求最多重试次数；省略默认 5，显式 0 = 不重试
	RequestTimeout  int      `json:"request_timeout"`   // 秒；上游未在超时内发出响应头则网关直接回 503
	BlockRetry      *bool    `json:"block_retry"`       // 安全拦截自动重试；省略字段时默认关闭
	MaxBlockRetries int      `json:"max_block_retries"` // 安全拦截自动重试次数上限（默认 0 = 关闭）
	BlockRetryMode  string   `json:"block_retry_mode"`  // full=完整缓冲检测（默认）| stream=只检测流式首块
	ProxyAuth       *bool    `json:"proxy_auth"`        // 代理转发鉴权；省略字段时默认开启（用 admin_password 作为访问密钥）
	Keys            []string `json:"keys"`
	AdminPassword   string   `json:"admin_password"` // WebUI/管理 API 的 Bearer 密码，留空时首次启动自动生成
}

// blockRetryEnabled 返回安全拦截自动重试开关；省略 block_retry 字段时默认关闭。
func (c *Config) blockRetryEnabled() bool {
	return c.BlockRetry != nil && *c.BlockRetry
}

// proxyAuthEnabled 返回代理转发鉴权开关；省略 proxy_auth 字段时默认开启，
// 避免网关在未鉴权状态下被暴露到公网后 API Key 被盗刷。
func (c *Config) proxyAuthEnabled() bool {
	return c.ProxyAuth == nil || *c.ProxyAuth
}

// maxRetries 返回最大重试次数；省略时取默认值，显式 0 表示不重试。
func (c *Config) maxRetries() int {
	if c.MaxRetries == nil {
		return defaultMaxRetry
	}
	return *c.MaxRetries
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.Upstream == "" {
		c.Upstream = defaultUpstream
	}
	if c.MaxRetries == nil {
		n := defaultMaxRetry
		c.MaxRetries = &n
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.MaxBlockRetries < 0 {
		c.MaxBlockRetries = defaultMaxBlockRetries
	}
	if c.BlockRetryMode == "" {
		c.BlockRetryMode = BlockRetryModeFull
	}
}

// validate 校验非 Key 配置字段。调用方需先 applyDefaults 归一化默认值。
func (c *Config) validate() error {
	if c.MaxRetries != nil && *c.MaxRetries < 0 {
		return fmt.Errorf("max_retries must be >= 0")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request_timeout must be > 0")
	}
	if c.MaxBlockRetries < 0 {
		return fmt.Errorf("max_block_retries must be >= 0")
	}
	if c.BlockRetryMode != BlockRetryModeFull && c.BlockRetryMode != BlockRetryModeStream {
		return fmt.Errorf("block_retry_mode must be %q or %q, got %q",
			BlockRetryModeFull, BlockRetryModeStream, c.BlockRetryMode)
	}
	return nil
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
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
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
// 所有写回都通过 configFileMu 串行化，避免并发读-改-写互相覆盖。
func updateConfig(path string, mutate func(map[string]any)) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()

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
