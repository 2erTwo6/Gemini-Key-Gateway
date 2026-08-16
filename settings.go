package main

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ConfigView 是 GET /api/config 的响应：当前生效配置的视图。
// admin_password 不原样返回，只报告是否已设置。
type ConfigView struct {
	Listen           string `json:"listen"`
	Upstream         string `json:"upstream"`
	MaxRetries       int    `json:"max_retries"`
	RequestTimeout   int    `json:"request_timeout"`
	BlockRetry       bool   `json:"block_retry"`
	MaxBlockRetries  int    `json:"max_block_retries"`
	BlockRetryMode   string `json:"block_retry_mode"`
	ProxyAuth        bool   `json:"proxy_auth"`
	AdminPasswordSet bool   `json:"admin_password_set"`
}

// ConfigUpdate 是 PUT /api/config 的请求体：所有字段均可选，只更新出现的字段。
type ConfigUpdate struct {
	Listen          *string `json:"listen"`
	Upstream        *string `json:"upstream"`
	MaxRetries      *int    `json:"max_retries"`
	RequestTimeout  *int    `json:"request_timeout"`
	BlockRetry      *bool   `json:"block_retry"`
	MaxBlockRetries *int    `json:"max_block_retries"`
	BlockRetryMode  *string `json:"block_retry_mode"`
	ProxyAuth       *bool   `json:"proxy_auth"`
	AdminPassword   *string `json:"admin_password"` // 空串 = 不修改
}

// ConfigApplyResult 是保存配置的结果。
type ConfigApplyResult struct {
	Ok              bool     `json:"ok"`
	Applied         []string `json:"applied"`          // 本次实际变化的字段
	RestartRequired []string `json:"restart_required"` // 需重启进程才生效的字段
	PasswordChanged bool     `json:"password_changed"`
	Persisted       bool     `json:"persisted"`
}

// SettingsManager 保存当前运行配置、持久化到 config.json，并把可热更新的
// 配置项实时应用到 Proxy / AuthGate。
type SettingsManager struct {
	mu       sync.RWMutex
	path     string
	cfg      Config
	pool     *Pool
	proxy    *Proxy
	authGate *AuthGate
}

func NewSettingsManager(path string, cfg *Config, pool *Pool, proxy *Proxy, authGate *AuthGate) *SettingsManager {
	c := *cfg
	return &SettingsManager{
		path:     path,
		cfg:      c,
		pool:     pool,
		proxy:    proxy,
		authGate: authGate,
	}
}

// View 返回当前配置视图（供 WebUI 表单回填）。
func (s *SettingsManager) View() ConfigView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return configView(&s.cfg)
}

// Apply 校验并保存配置。持久化成功后才应用运行时热更新。
func (s *SettingsManager) Apply(u ConfigUpdate) (ConfigApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.cfg // 拷贝一份，全部校验通过后再落库
	changed := make([]string, 0, 9)
	passwordChanged := false

	if u.Listen != nil {
		v := strings.TrimSpace(*u.Listen)
		if v != cfg.Listen {
			cfg.Listen = v
			changed = append(changed, "listen")
		}
	}
	if u.Upstream != nil {
		v := strings.TrimSpace(*u.Upstream)
		if v != cfg.Upstream {
			cfg.Upstream = v
			changed = append(changed, "upstream")
		}
	}
	if u.MaxRetries != nil {
		v := *u.MaxRetries
		if v < 0 {
			return ConfigApplyResult{}, fmt.Errorf("max_retries must be >= 0")
		}
		if cfg.MaxRetries == nil || *cfg.MaxRetries != v {
			n := v
			cfg.MaxRetries = &n
			changed = append(changed, "max_retries")
		}
	}
	if u.RequestTimeout != nil {
		v := *u.RequestTimeout
		if v <= 0 {
			return ConfigApplyResult{}, fmt.Errorf("request_timeout must be > 0")
		}
		if v != cfg.RequestTimeout {
			cfg.RequestTimeout = v
			changed = append(changed, "request_timeout")
		}
	}
	if u.BlockRetry != nil {
		v := *u.BlockRetry
		if cfg.BlockRetry == nil || *cfg.BlockRetry != v {
			b := v
			cfg.BlockRetry = &b
			changed = append(changed, "block_retry")
		}
	}
	if u.MaxBlockRetries != nil {
		v := *u.MaxBlockRetries
		if v < 0 {
			return ConfigApplyResult{}, fmt.Errorf("max_block_retries must be >= 0")
		}
		if v != cfg.MaxBlockRetries {
			cfg.MaxBlockRetries = v
			changed = append(changed, "max_block_retries")
		}
	}
	if u.BlockRetryMode != nil {
		v := *u.BlockRetryMode
		if v != cfg.BlockRetryMode {
			cfg.BlockRetryMode = v
			changed = append(changed, "block_retry_mode")
		}
	}
	if u.ProxyAuth != nil {
		v := *u.ProxyAuth
		if cfg.ProxyAuth == nil || *cfg.ProxyAuth != v {
			b := v
			cfg.ProxyAuth = &b
			changed = append(changed, "proxy_auth")
		}
	}
	if u.AdminPassword != nil && *u.AdminPassword != "" {
		v := *u.AdminPassword
		if v != cfg.AdminPassword {
			cfg.AdminPassword = v
			passwordChanged = true
			changed = append(changed, "admin_password")
		}
	}

	if len(changed) == 0 {
		return ConfigApplyResult{Ok: true}, nil
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return ConfigApplyResult{}, err
	}
	if u, err := url.Parse(cfg.Upstream); err != nil || cfg.Upstream == "" || u.Scheme == "" || u.Host == "" {
		return ConfigApplyResult{}, fmt.Errorf("upstream %q is not a valid absolute URL (e.g. https://host)", cfg.Upstream)
	}

	// 先持久化，写盘失败就不改运行时状态。
	if err := s.persist(&cfg); err != nil {
		return ConfigApplyResult{}, fmt.Errorf("persist config: %w", err)
	}

	// 运行时热更新。除 listen 外，其余配置均立即生效。
	if s.proxy != nil {
		if err := s.proxy.SetUpstream(cfg.Upstream); err != nil {
			return ConfigApplyResult{}, err
		}
		s.proxy.SetMaxRetries(cfg.maxRetries())
		s.proxy.SetRequestTimeout(time.Duration(cfg.RequestTimeout) * time.Second)
		s.proxy.SetBlockRetry(cfg.blockRetryEnabled(), cfg.MaxBlockRetries, cfg.BlockRetryMode)
		if cfg.proxyAuthEnabled() {
			s.proxy.SetAuthKey(cfg.AdminPassword)
		} else {
			s.proxy.SetAuthKey("")
		}
	}
	if passwordChanged && s.authGate != nil {
		s.authGate.SetPassword(cfg.AdminPassword)
	}

	s.cfg = cfg

	res := ConfigApplyResult{
		Ok:              true,
		Applied:         changed,
		PasswordChanged: passwordChanged,
		Persisted:       true,
	}
	// 监听地址只能通过重启进程生效。
	for _, name := range changed {
		if name == "listen" {
			res.RestartRequired = append(res.RestartRequired, "listen")
		}
	}
	return res, nil
}

// persist 将当前配置（含池中 Key）写回配置文件，保留未知字段。
func (s *SettingsManager) persist(cfg *Config) error {
	keys := s.pool.Keys()
	return updateConfig(s.path, func(m map[string]any) {
		m["listen"] = cfg.Listen
		m["upstream"] = cfg.Upstream
		m["max_retries"] = cfg.maxRetries()
		m["request_timeout"] = cfg.RequestTimeout
		m["block_retry"] = cfg.blockRetryEnabled()
		m["max_block_retries"] = cfg.MaxBlockRetries
		m["block_retry_mode"] = cfg.BlockRetryMode
		m["proxy_auth"] = cfg.proxyAuthEnabled()
		if cfg.AdminPassword != "" {
			m["admin_password"] = cfg.AdminPassword
		}
		m["keys"] = keys
	})
}

func configView(c *Config) ConfigView {
	return ConfigView{
		Listen:           c.Listen,
		Upstream:         c.Upstream,
		MaxRetries:       c.maxRetries(),
		RequestTimeout:   c.RequestTimeout,
		BlockRetry:       c.blockRetryEnabled(),
		MaxBlockRetries:  c.MaxBlockRetries,
		BlockRetryMode:   c.BlockRetryMode,
		ProxyAuth:        c.proxyAuthEnabled(),
		AdminPasswordSet: c.AdminPassword != "",
	}
}
