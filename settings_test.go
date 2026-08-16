package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSettingsTestEnv(t *testing.T) (*Pool, *Proxy, *AuthGate, *SettingsManager, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"listen": ":8080",
		"upstream": "https://generativelanguage.googleapis.com",
		"max_retries": 5,
		"request_timeout": 30,
		"block_retry": false,
		"max_block_retries": 0,
		"block_retry_mode": "full",
		"proxy_auth": true,
		"admin_password": "secret",
		"keys": ["k1"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewPool(cfg.Keys)
	proxy := NewProxy(pool, cfg.Upstream, cfg.maxRetries(), time.Duration(cfg.RequestTimeout)*time.Second)
	proxy.SetBlockRetry(cfg.blockRetryEnabled(), cfg.MaxBlockRetries, cfg.BlockRetryMode)
	proxy.SetAuthKey(cfg.AdminPassword)
	gate := NewAuthGate(cfg.AdminPassword)
	settings := NewSettingsManager(cfgPath, cfg, pool, proxy, gate)
	return pool, proxy, gate, settings, cfgPath
}

func TestSettingsManagerApplyLiveAndPersist(t *testing.T) {
	_, proxy, gate, settings, cfgPath := newSettingsTestEnv(t)

	maxRetries := 3
	timeout := 45
	on := true
	off := false
	mode := BlockRetryModeStream
	newPassword := "new-secret"
	res, err := settings.Apply(ConfigUpdate{
		Upstream:        strPtr("https://example.invalid"),
		MaxRetries:      &maxRetries,
		RequestTimeout:  &timeout,
		BlockRetry:      &on,
		MaxBlockRetries: intPtr(2),
		BlockRetryMode:  &mode,
		ProxyAuth:       &off,
		AdminPassword:   &newPassword,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Ok || !res.Persisted || !res.PasswordChanged {
		t.Errorf("Apply result = %+v, want ok/persisted/password_changed", res)
	}
	if len(res.RestartRequired) != 0 {
		t.Errorf("only listen should require restart, got %v", res.RestartRequired)
	}

	snap := proxy.configSnapshot()
	if snap.maxRetries != 3 || !snap.blockRetry || snap.maxBlockRetries != 2 || snap.blockRetryMode != BlockRetryModeStream {
		t.Errorf("proxy snapshot = %+v, want live values applied", snap)
	}
	if got := proxy.authKey; got != "" {
		t.Errorf("proxy authKey = %q, want empty (proxy_auth off)", got)
	}
	if got := gate.Password(); got != "new-secret" {
		t.Errorf("gate password = %q, want new-secret", got)
	}

	// 配置文件应写入新值并保留 keys
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"max_retries": 3`, `"request_timeout": 45`, `"block_retry": true`, `"block_retry_mode": "stream"`, `"proxy_auth": false`, `"admin_password": "new-secret"`, `"k1"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config file missing %s, got: %s", want, raw)
		}
	}
}

func TestSettingsManagerRejectsInvalid(t *testing.T) {
	_, _, _, settings, _ := newSettingsTestEnv(t)

	badTimeout := 0
	if _, err := settings.Apply(ConfigUpdate{RequestTimeout: &badTimeout}); err == nil {
		t.Error("request_timeout=0 should be rejected")
	}
	badMode := "weird"
	if _, err := settings.Apply(ConfigUpdate{BlockRetryMode: &badMode}); err == nil {
		t.Error("bad block_retry_mode should be rejected")
	}
	badUpstream := "not-a-url"
	if _, err := settings.Apply(ConfigUpdate{Upstream: &badUpstream}); err == nil {
		t.Error("bad upstream should be rejected")
	}
	badMax := -1
	if _, err := settings.Apply(ConfigUpdate{MaxRetries: &badMax}); err == nil {
		t.Error("negative max_retries should be rejected")
	}
}

func TestSettingsConfigAPI(t *testing.T) {
	_, proxy, gate, settings, _ := newSettingsTestEnv(t)
	web := &WebUI{pool: proxy.pool, adminPassword: "secret", auth: gate, settings: settings}

	mux := http.NewServeMux()
	mux.Handle("GET /api/config", gate.Wrap(http.HandlerFunc(web.handleConfig)))
	mux.Handle("PUT /api/config", gate.Wrap(http.HandlerFunc(web.handleConfig)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// GET：需认证，且不返回密码明文
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var view ConfigView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !view.AdminPasswordSet {
		t.Error("admin_password_set should be true")
	}
	if strings.Contains(mustJSON(t, view), "secret") {
		t.Error("config view must not leak admin password")
	}

	// PUT：更新 max_retries 并校验立即生效
	body := `{"max_retries":2}`
	req2, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer secret")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d, want 200", resp2.StatusCode)
	}
	if got := proxy.configSnapshot().maxRetries; got != 2 {
		t.Errorf("maxRetries after PUT = %d, want 2", got)
	}

	// 无 token → 401
	resp3, err := http.Get(srv.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/config without token status = %d, want 401", resp3.StatusCode)
	}
}

func TestSettingsListenRequiresRestart(t *testing.T) {
	_, _, _, settings, _ := newSettingsTestEnv(t)
	res, err := settings.Apply(ConfigUpdate{Listen: strPtr("127.0.0.1:9999")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RestartRequired) != 1 || res.RestartRequired[0] != "listen" {
		t.Errorf("RestartRequired = %v, want [listen]", res.RestartRequired)
	}
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
