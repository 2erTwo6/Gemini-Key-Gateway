package main

import (
	"embed"
	"encoding/json"
	"net/http"
)

//go:embed web/index.html
var webFS embed.FS

type WebUI struct {
	pool          *Pool
	adminPassword string
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

func (w *WebUI) handleAddKey(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "key is required"})
		return
	}
	id := w.pool.Add(req.Key)
	writeJSON(rw, http.StatusOK, map[string]string{"id": id})
}

func (w *WebUI) handleDeleteKey(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !w.pool.Remove(id) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"ok": "true"})
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
