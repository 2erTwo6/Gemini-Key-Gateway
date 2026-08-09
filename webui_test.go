package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newWebUI(t *testing.T) *httptest.Server {
	t.Helper()
	pool := NewPool([]string{"k1"})
	web := &WebUI{pool: pool, adminPassword: "secret"}

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
