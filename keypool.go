package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
	_ "time/tzdata" // 内嵌时区数据，保持零依赖
)

type KeyState int

const (
	StateAvailable KeyState = iota
	StateInvalid            // 4xx（429 除外）永久失效
	StateDisabled           // WebUI 手动禁用
)

type LockKind string

const (
	LockRPD LockKind = "rpd" // 锁定到当日额度刷新点
	LockRPM LockKind = "rpm" // 固定冷却 defaultRPMLock 秒
	LockTPM LockKind = "tpm" // token 超限：固定冷却 defaultRPMLock 秒，不换 Key 重试
)

type modelLock struct {
	kind  LockKind
	until time.Time
}

// Key 池内单个 Key 的运行时状态。
// 并发规则：所有字段仅在持锁时读写（Requests 等计数也在持锁时递增，
// 见 Pick/MarkInvalid/LockModel），保证状态一致。
type Key struct {
	id        string
	key       string
	state     KeyState
	modelLock map[string]modelLock // model -> 锁定信息（过期即视为无锁，懒清理）
	requests  int64
	failures  int64
	lastError string
	lastUsed  time.Time
}

type Pool struct {
	mu          sync.RWMutex
	keys        []*Key
	rr          int
	nowFn       func() time.Time
	refreshFn   func(time.Time) time.Time
	rpmLockDur  time.Duration
	rpmLockKind LockKind
}

type PoolOpt func(*Pool)

func WithClock(now func() time.Time) PoolOpt {
	return func(p *Pool) { p.nowFn = now }
}

func WithRefresh(fn func(time.Time) time.Time) PoolOpt {
	return func(p *Pool) { p.refreshFn = fn }
}

func WithRPMLock(d time.Duration) PoolOpt {
	return func(p *Pool) { p.rpmLockDur = d }
}

func NewPool(keys []string, opts ...PoolOpt) *Pool {
	p := &Pool{
		nowFn:      time.Now,
		refreshFn:  nextRefreshTime,
		rpmLockDur: defaultRPMLock * time.Second,
	}
	for _, o := range opts {
		o(p)
	}
	for _, k := range keys {
		p.keys = append(p.keys, &Key{
			id:        shortID(k),
			key:       k,
			state:     StateAvailable,
			modelLock: make(map[string]modelLock),
		})
	}
	return p
}

// nextRefreshTime 返回 Gemini RPD 额度的下一个刷新时刻：
// 美国太平洋时间当日午夜（PT 00:00）+1s 缓冲。
// 等价于北京时间夏令时 15:00 / 冬令时 16:00，自动适配 DST。
func nextRefreshTime(t time.Time) time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return t.Add(24 * time.Hour)
	}
	y, m, d := t.In(loc).Date()
	next := time.Date(y, m, d+1, 0, 0, 1, 0, loc) // 次日 00:00:01 PT
	return next
}

func shortID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:10]
}

// Pick 按 model 过滤出可用 Key 后 round-robin 选取。
// 返回 nil 表示无可用 Key。选中的 Key 已计入请求数。
func (p *Pool) Pick(model string) *Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.nowFn()
	n := len(p.keys)
	if n == 0 {
		return nil
	}
	start := p.rr
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		k := p.keys[idx]
		if !k.usable(model, now) {
			continue
		}
		p.rr = (idx + 1) % n
		k.requests++
		k.lastUsed = now
		return k
	}
	return nil
}

func (k *Key) usable(model string, now time.Time) bool {
	if k.state != StateAvailable {
		return false
	}
	if model != "" {
		if lock, ok := k.modelLock[model]; ok {
			if lock.until.After(now) {
				return false
			}
			delete(k.modelLock, model) // 懒清理过期锁
		}
	}
	return true
}

// MarkInvalid 4xx（429 除外）→ 永久失效。
func (p *Pool) MarkInvalid(id string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k := p.byIDLocked(id); k != nil {
		k.state = StateInvalid
		k.failures++
		k.lastError = "4xx: " + httpStatus(status)
	}
}

// RecordFailure 5xx / 网络错误：不标记状态，仅记录。
func (p *Pool) RecordFailure(id string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k := p.byIDLocked(id); k != nil {
		k.failures++
		k.lastError = "5xx: " + httpStatus(status)
	}
}

// LockModel 429：按类型锁定 Key×Model。
// RPD 锁到下一刷新点；RPM/TPM 锁 rpmLockDur。只延长不缩短已有锁。
func (p *Pool) LockModel(id, model string, kind LockKind) {
	if model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byIDLocked(id)
	if k == nil {
		return
	}
	now := p.nowFn()
	var until time.Time
	if kind == LockRPD {
		until = p.refreshFn(now)
	} else {
		until = now.Add(p.rpmLockDur)
	}
	if prev, ok := k.modelLock[model]; ok && prev.until.After(until) {
		return
	}
	k.modelLock[model] = modelLock{kind: kind, until: until}
	k.failures++
	k.lastError = "429: " + string(kind)
}

// SetState 手动启用/禁用。启用会清空该 Key 全部模型锁并恢复可用。
func (p *Pool) SetState(id string, s KeyState) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.byIDLocked(id)
	if k == nil {
		return false
	}
	k.state = s
	if s == StateAvailable {
		k.modelLock = make(map[string]modelLock)
		k.lastError = ""
	}
	return true
}

// Add 添加 Key；返回其 ID 以及是否为新加入（false = 池中已存在，仅返回既有 ID）。
func (p *Pool) Add(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := shortID(key)
	for _, k := range p.keys {
		if k.key == key {
			return k.id, false
		}
	}
	p.keys = append(p.keys, &Key{
		id:        id,
		key:       key,
		state:     StateAvailable,
		modelLock: make(map[string]modelLock),
	})
	return id, true
}

func (p *Pool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, k := range p.keys {
		if k.id == id {
			p.keys = append(p.keys[:i], p.keys[i+1:]...)
			return true
		}
	}
	return false
}

// Keys 返回池内全部原始 Key（按添加顺序）。
func (p *Pool) Keys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	keys := make([]string, 0, len(p.keys))
	for _, k := range p.keys {
		keys = append(keys, k.key)
	}
	return keys
}

func (p *Pool) byIDLocked(id string) *Key {
	for _, k := range p.keys {
		if k.id == id {
			return k
		}
	}
	return nil
}

type KeyLock struct {
	Kind  string    `json:"kind"` // rpd | rpm
	Until time.Time `json:"until"`
}

type KeyInfo struct {
	ID        string             `json:"id"`
	Key       string             `json:"key"` // 打码显示
	State     string             `json:"state"`
	Locks     map[string]KeyLock `json:"locks"` // model -> 锁定信息
	Requests  int64              `json:"requests"`
	Failures  int64              `json:"failures"`
	LastError string             `json:"last_error"`
	LastUsed  *time.Time         `json:"last_used"`
}

type PoolSnapshot struct {
	Total       int       `json:"total"`
	Available   int       `json:"available"`
	Invalid     int       `json:"invalid"`
	Disabled    int       `json:"disabled"`
	LockedModel int       `json:"locked_models"`
	RefreshTime time.Time `json:"refresh_time"` // 下一个 RPD 赦免时刻
	Keys        []KeyInfo `json:"keys"`
}

func (p *Pool) Snapshot() PoolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := p.nowFn()
	snap := PoolSnapshot{
		Total:       len(p.keys),
		RefreshTime: p.refreshFn(now),
		Keys:        make([]KeyInfo, 0, len(p.keys)),
	}
	for _, k := range p.keys {
		info := KeyInfo{
			ID:        k.id,
			Key:       maskKey(k.key),
			State:     stateName(k.state),
			Locks:     make(map[string]KeyLock),
			Requests:  k.requests,
			Failures:  k.failures,
			LastError: k.lastError,
		}
		for model, lock := range k.modelLock {
			if lock.until.After(now) {
				info.Locks[model] = KeyLock{Kind: string(lock.kind), Until: lock.until}
				snap.LockedModel++
			}
		}
		switch k.state {
		case StateAvailable:
			snap.Available++
		case StateInvalid:
			snap.Invalid++
		case StateDisabled:
			snap.Disabled++
		}
		if !k.lastUsed.IsZero() {
			t := k.lastUsed
			info.LastUsed = &t
		}
		snap.Keys = append(snap.Keys, info)
	}
	return snap
}

func stateName(s KeyState) string {
	switch s {
	case StateAvailable:
		return "available"
	case StateInvalid:
		return "invalid"
	case StateDisabled:
		return "disabled"
	}
	return "unknown"
}

func maskKey(k string) string {
	if len(k) <= 10 {
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}

func httpStatus(code int) string {
	if code <= 0 {
		return "network error"
	}
	return fmt.Sprintf("%d", code)
}
