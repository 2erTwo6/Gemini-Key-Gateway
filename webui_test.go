package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWebUI(t *testing.T) *httptest.Server {
	t.Helper()
	pool := NewPool([]string{"k1"})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"listen":":8080","keys":["k1"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	web := &WebUI{pool: pool, adminPassword: "secret", configPath: cfgPath}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", web.handleHealth) // 探针免认证
	mux.HandleFunc("POST /api/login", web.handleLogin)
	mux.Handle("GET /api/pool", bearerAuth(http.HandlerFunc(web.handlePool), "secret"))
	mux.Handle("POST /api/keys", bearerAuth(http.HandlerFunc(web.handleAddKey), "secret"))
	mux.Handle("DELETE /api/keys/{id}", bearerAuth(http.HandlerFunc(web.handleDeleteKey), "secret"))
	mux.Handle("POST /api/keys/{id}/state", bearerAuth(http.HandlerFunc(web.handleSetState), "secret"))
	mux.HandleFunc("/", web.handleIndex)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, url, body string, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestWebUILoginAndTokenAuth(t *testing.T) {
	srv := newWebUI(t)

	// 页面公开（只是 HTML）
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("index status = %d, want 200", resp.StatusCode)
	}

	// 无 token 访问 API → 401
	resp2, _ := http.Get(srv.URL + "/api/pool")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", resp2.StatusCode)
	}

	// 错密码登录 → 401
	resp3, _ := postJSON(t, srv.URL+"/api/login", `{"password":"wrong"}`, "")
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", resp3.StatusCode)
	}

	// 正确密码登录 → token
	resp4, body4 := postJSON(t, srv.URL+"/api/login", `{"password":"secret"}`, "")
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("login: status = %d, want 200", resp4.StatusCode)
	}
	if !strings.Contains(body4, `"token"`) {
		t.Errorf("login response = %q, want token", body4)
	}

	// 带 token 访问 → 200
	req5, _ := http.NewRequest("GET", srv.URL+"/api/pool", nil)
	req5.Header.Set("Authorization", "Bearer secret")
	resp5, err := http.DefaultClient.Do(req5)
	if err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != http.StatusOK {
		t.Errorf("with token: status = %d, want 200", resp5.StatusCode)
	}
}

func TestHealthNoAuth(t *testing.T) {
	srv := newWebUI(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health status = %d, want 200 (probe must be auth-free)", resp.StatusCode)
	}
}

func TestAddKeyBatchAndPersist(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"listen":":8080","keys":["k1"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pool := NewPool([]string{"k1"})
	web := &WebUI{pool: pool, adminPassword: "secret", configPath: cfgPath}
	mux := http.NewServeMux()
	mux.Handle("POST /api/keys", bearerAuth(http.HandlerFunc(web.handleAddKey), "secret"))
	mux.Handle("DELETE /api/keys/{id}", bearerAuth(http.HandlerFunc(web.handleDeleteKey), "secret"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// 批量添加：去重 + 跳过空串
	resp, body := postJSON(t, srv.URL+"/api/keys", `{"keys":["k2","k3"," k2 ",""]}`, "secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch add: status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, `"added":2`) || !strings.Contains(body, `"persisted":true`) {
		t.Errorf("batch add response = %q, want added=2 persisted=true", body)
	}
	// 池内生效
	snap := pool.Snapshot()
	if snap.Total != 3 {
		t.Errorf("pool total = %d, want 3", snap.Total)
	}

	// 已持久化到配置文件（保留 listen 等其余字段）
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"k1"`, `"k2"`, `"k3"`, `"listen": ":8080"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("config file missing %s, got: %s", want, raw)
		}
	}

	// 旧格式单条添加仍兼容
	resp2, body2 := postJSON(t, srv.URL+"/api/keys", `{"key":"k4"}`, "secret")
	if resp2.StatusCode != http.StatusOK || !strings.Contains(body2, `"id"`) {
		t.Errorf("single add: status=%d body=%q", resp2.StatusCode, body2)
	}

	// 删除也持久化
	req3, _ := http.NewRequest("DELETE", srv.URL+"/api/keys/"+pool.Snapshot().Keys[1].ID, nil)
	req3.Header.Set("Authorization", "Bearer secret")
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("delete: status = %d, want 200", resp3.StatusCode)
	}
	raw2, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw2), `"k2"`) {
		t.Errorf("config file should not contain deleted key, got: %s", raw2)
	}
}
