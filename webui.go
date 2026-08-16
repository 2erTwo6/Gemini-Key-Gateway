package main

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// AuthGate 是线程安全的管理密码门禁。WebUI 登录、管理 API 的 Bearer 认证
// 以及代理转发鉴权（经 Proxy.SetAuthKey 同步）共用同一密码；WebUI 修改
// 密码后调用 SetPassword 即可立即生效，无需重启。
type AuthGate struct {
	mu       sync.RWMutex
	password string
}

func NewAuthGate(password string) *AuthGate {
	return &AuthGate{password: password}
}

func (g *AuthGate) Password() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.password
}

func (g *AuthGate) SetPassword(p string) {
	g.mu.Lock()
	g.password = p
	g.mu.Unlock()
}

func (g *AuthGate) CheckPassword(p string) bool {
	g.mu.RLock()
	cur := g.password
	g.mu.RUnlock()
	return constantTimeEq(p, cur)
}

// Wrap 给管理 API 套 Bearer Token 认证。
func (g *AuthGate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" || !g.CheckPassword(token) {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// bearerToken 提取 Authorization: Bearer <token>。
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return strings.TrimSpace(v[len(prefix):])
	}
	return ""
}

type WebUI struct {
	pool          *Pool
	adminPassword string // 兼容测试/简单场景；auth 非空时以 auth 为准
	auth          *AuthGate
	settings      *SettingsManager
	configPath    string        // 配置文件路径；WebUI 增删 Key 后写回（持久化）
	restartFn     func() error  // 测试可注入；nil 时使用 restartProcess
	restartDelay  time.Duration // 重启前的延迟，默认 200ms；测试可注入更短值
}

// currentPassword 返回当前生效的管理密码。
func (w *WebUI) currentPassword() string {
	if w.auth != nil {
		return w.auth.Password()
	}
	return w.adminPassword
}

// checkPassword 校验管理密码（恒定时间比较）。
func (w *WebUI) checkPassword(pw string) bool {
	if w.auth != nil {
		return w.auth.CheckPassword(pw)
	}
	return constantTimeEq(pw, w.adminPassword)
}

func (w *WebUI) handleLogin(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	if !w.checkPassword(req.Password) {
		writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"token": w.currentPassword()})
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
		id, isNew := w.pool.Add(k)
		if isNew {
			added++
		}
		ids = append(ids, id)
	}
	resp := map[string]any{
		"added":     added,
		"ids":       ids,
		"persisted": w.persistKeys(),
	}
	if len(keys) == 1 && len(ids) > 0 {
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

// handleConfig 读取 / 更新运行配置。
// GET 返回当前配置视图（admin_password 不返回明文）；PUT/POST 接受 ConfigUpdate，
// 持久化到配置文件并热更新可即时生效的项。
func (w *WebUI) handleConfig(rw http.ResponseWriter, r *http.Request) {
	if w.settings == nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]string{"error": "settings manager unavailable"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(rw, http.StatusOK, w.settings.View())
	case http.MethodPut, http.MethodPost:
		var u ConfigUpdate
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		res, err := w.settings.Apply(u)
		if err != nil {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(rw, http.StatusOK, res)
	default:
		rw.Header().Set("Allow", "GET, PUT, POST")
		writeJSON(rw, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleRestart 重启进程：先返回响应，短暂延迟后重新执行当前可执行文件
// （syscall.Exec，PID 不变，Docker 容器内同样适用）。
func (w *WebUI) handleRestart(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", "POST")
		writeJSON(rw, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": "true", "message": "restarting"})
	if f, ok := rw.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		delay := w.restartDelay
		if delay <= 0 {
			delay = 200 * time.Millisecond
		}
		time.Sleep(delay)
		fn := w.restartFn
		if fn == nil {
			fn = restartProcess
		}
		if err := fn(); err != nil {
			slog.Error("process restart failed", "err", err)
		}
	}()
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

// bearerAuth 管理接口的 Bearer Token 认证（token 即配置的 admin_password）。
// 兼容旧测试；生产路径请使用 AuthGate.Wrap，以便 WebUI 修改密码后即时生效。
func bearerAuth(next http.Handler, password string) http.Handler {
	return NewAuthGate(password).Wrap(next)
}
