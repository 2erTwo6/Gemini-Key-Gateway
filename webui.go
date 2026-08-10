package main

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed web/index.html
var webFS embed.FS

type WebUI struct {
	pool          *Pool
	adminPassword string
	configPath    string // 配置文件路径；WebUI 增删 Key 后写回（持久化）
}

func (w *WebUI) handleLogin(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	if req.Password != w.adminPassword {
		writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"token": req.Password})
}

func (w *WebUI) handleIndex(rw http.ResponseWriter, r *http.Request) {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(rw, "webui asset missing", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Write(raw)
}

func (w *WebUI) handleHealth(rw http.ResponseWriter, r *http.Request) {
	s := w.pool.Snapshot()
	writeJSON(rw, http.StatusOK, map[string]any{
		"status":    "ok",
		"total":     s.Total,
		"available": s.Available,
		"invalid":   s.Invalid,
		"disabled":  s.Disabled,
		"locked":    s.LockedModel,
		"refresh":   s.RefreshTime,
	})
}

func (w *WebUI) handlePool(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, w.pool.Snapshot())
}

// handleAddKey 添加单个（{"key": "..."}）或批量（{"keys": ["...", "..."]}）Key，
// 支持在 keys 数组中混入空串/重复项（自动去重跳过），添加后写回配置文件。
func (w *WebUI) handleAddKey(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Key  string   `json:"key"`
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	keys := req.Keys
	if len(keys) == 0 {
		if req.Key == "" {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "key is required"})
			return
		}
		keys = []string{req.Key}
	}
	seen := make(map[string]bool)
	ids := make([]string, 0, len(keys))
	added := 0
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		added++
		ids = append(ids, w.pool.Add(k))
	}
	resp := map[string]any{
		"added":     added,
		"ids":       ids,
		"persisted": w.persistKeys(),
	}
	if len(keys) == 1 {
		resp["id"] = ids[0]
	}
	writeJSON(rw, http.StatusOK, resp)
}

// persistKeys 将当前池内全部 Key 写回配置文件；未配置 configPath 时跳过。
func (w *WebUI) persistKeys() bool {
	if w.configPath == "" {
		return false
	}
	if err := saveConfigKeys(w.configPath, w.pool.Keys()); err != nil {
		slog.Error("persist keys to config", "err", err)
		return false
	}
	return true
}

func (w *WebUI) handleDeleteKey(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !w.pool.Remove(id) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": "true", "persisted": w.persistKeys()})
}

func (w *WebUI) handleSetState(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	var s KeyState
	switch req.State {
	case "enabled":
		s = StateAvailable
	case "disabled":
		s = StateDisabled
	default:
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "state must be enabled|disabled"})
		return
	}
	if !w.pool.SetState(id, s) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"ok": "true"})
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

// bearerAuth 管理接口的 Bearer Token 认证（token 即配置的 admin_password）。
func bearerAuth(next http.Handler, password string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+password {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(rw, r)
	})
}
