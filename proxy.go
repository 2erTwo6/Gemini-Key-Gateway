package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	"Host",
}

type Proxy struct {
	pool *Pool

	clientMu sync.RWMutex
	client   *http.Client

	upstreamMu sync.RWMutex
	upstream   *url.URL

	// cfgMu 保护下列可由 WebUI 运行时热更新的配置项。
	cfgMu           sync.RWMutex
	maxRetries      int
	blockRetry      bool
	maxBlockRetries int
	blockRetryMode  string
	authKey         string // 非空时启用代理转发鉴权：客户端必须携带该密钥（通常为 admin_password）
}

// proxyConfig 是一次代理请求在启动时读取的运行时配置快照，
// 保证同一请求内配置一致，避免热更新读到一半。
type proxyConfig struct {
	maxRetries      int
	blockRetry      bool
	maxBlockRetries int
	blockRetryMode  string
}

func NewProxy(pool *Pool, upstream string, maxRetries int, requestTimeout time.Duration) *Proxy {
	u, err := url.Parse(upstream)
	if err != nil {
		u, _ = url.Parse(defaultUpstream)
	}
	return &Proxy{
		pool:       pool,
		client:     newHTTPClient(requestTimeout),
		upstream:   u,
		maxRetries: maxRetries,
	}
}

func newHTTPClient(requestTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		DisableCompression:    true, // 不干预编码，保证透传忠实
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
	}
	return &http.Client{Transport: tr}
}

// SetAuthKey 设置代理转发的访问密钥。key 非空时，所有 /v1beta 请求必须先通过
// requestAuthorized 鉴权（x-goog-api-key 头或 Authorization: Bearer），
// 否则直接返回 401，不读取请求体、不触碰 Key 池。key 为空表示不启用鉴权。
func (p *Proxy) SetAuthKey(key string) {
	p.cfgMu.Lock()
	p.authKey = key
	p.cfgMu.Unlock()
}

// SetMaxRetries 热更新最大重试次数；负数按 0 处理（不重试）。
func (p *Proxy) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	p.cfgMu.Lock()
	p.maxRetries = n
	p.cfgMu.Unlock()
}

// SetUpstream 热更新上游地址，立即对后续请求生效（已在途请求不受影响）。
func (p *Proxy) SetUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid upstream %q: must be an absolute URL like https://host", raw)
	}
	p.upstreamMu.Lock()
	p.upstream = u
	p.upstreamMu.Unlock()
	return nil
}

// SetRequestTimeout 热更新上游响应头超时：替换 HTTP client（旧 client 的空闲连接会被关闭），
// 已在途请求继续使用旧 client，后续请求使用新超时。
func (p *Proxy) SetRequestTimeout(d time.Duration) {
	p.clientMu.Lock()
	old := p.client
	p.client = newHTTPClient(d)
	p.clientMu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

// SetBlockRetry 配置安全拦截自动重试：检测到 content 端点响应被安全机制拦截
// （promptFeedback.blockReason 非空或候选 finishReason 为 SAFETY 等）时，在请求体
// contents 末尾追加一条「user: EOF」消息后，像普通重试一样从 Key 池
// Pick 下一个可用 Key 重试，
// 最多重试 maxRetries 次（默认 1）。这利用上游「只检查最后一条 user 消息」的特性
// 减少误报，并非真正绕过安全机制。
//
// mode 可选 BlockRetryModeFull（默认）或 BlockRetryModeStream：
//   - full：完整缓冲 2xx 响应判定拦截，能发现流中途的 SAFETY 截断，但流式首字节延迟。
//   - stream：只检查流式响应首块（SSE 首事件 / JSON 数组首元素），未拦截立即透传，
//     保持流式实时性；流中途被安全截断不再重试，由客户端自行处理。
func (p *Proxy) SetBlockRetry(enabled bool, maxRetries int, modes ...string) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	mode := BlockRetryModeFull
	if len(modes) > 0 && modes[0] != "" {
		mode = modes[0]
	}
	p.cfgMu.Lock()
	p.blockRetry = enabled
	p.maxBlockRetries = maxRetries
	p.blockRetryMode = mode
	p.cfgMu.Unlock()
}

// configSnapshot 读取当前运行时配置快照。
func (p *Proxy) configSnapshot() proxyConfig {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return proxyConfig{
		maxRetries:      p.maxRetries,
		blockRetry:      p.blockRetry,
		maxBlockRetries: p.maxBlockRetries,
		blockRetryMode:  p.blockRetryMode,
	}
}

// ServeHTTP 转发到上游：轮询可用 Key、按响应分类重试、安全拦截自动重试、最后透传。
// 请求体读入内存（重试需重放）；响应流式透传，零缓冲改写。
// 注意：开启 blockRetry 且 mode=full（默认）时，content 端点（generateContent/streamGenerateContent）
// 的 2xx 响应会先整体读入内存以判定是否被安全拦截，未拦截时再逐字节透传（流式首字节因此后移）。
// mode=stream 时只缓冲流式响应首块做判定，未拦截立即透传，保持流式实时性。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 代理转发鉴权：默认开启（authKey 由 main 注入 admin_password）。
	// 在读取请求体、挑选 Key 之前拦截，未授权请求不触碰任何上游资源。
	p.cfgMu.RLock()
	authKey := p.authKey
	p.cfgMu.RUnlock()
	if authKey != "" && !requestAuthorized(r, authKey) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gemini-key-gateway"`)
		http.Error(w, "unauthorized: invalid gateway key", http.StatusUnauthorized)
		return
	}

	cfg := p.configSnapshot()
	model := extractModel(r.URL.Path)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("read request body", "err", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var last *http.Response
	blockRetries := 0
	for attempt := 0; attempt <= cfg.maxRetries; attempt++ {
		key := p.pool.Pick(model)
		if key == nil {
			slog.Warn("no usable key", "model", model, "attempt", attempt)
			break
		}
		if last != nil {
			// 即将发起下一次尝试，旧的“最后一次响应”不再需要，关闭以复用连接。
			last.Body.Close()
			last = nil
		}
		resp, err := p.forward(r.Context(), r.Method, r.URL, body, r.Header, key.key, cfg)
		if err != nil {
			p.pool.RecordFailure(key.id, 0)
			slog.Warn("upstream request failed, returning 503", "key", key.id, "model", model, "err", err)
			// 网络错误/超时不重试（上游同一端点，换 Key 无意义），重试交给下游；
			// 客户端已断开则无处可写，直接返回。
			if r.Context().Err() != nil {
				return
			}
			http.Error(w, "The Gemini API did not provide any response before timing out.", http.StatusServiceUnavailable)
			return
		}

		// 安全拦截自动重试（仅 content 生成端点）：2xx 但被安全机制拦截时，在请求体
		// contents 末尾追加一条「user: EOF」，然后像普通重试一样回到外层循环，
		// 从 Key 池中 Pick 下一个可用 Key，不绑定上一个 Key。
		if resp.StatusCode >= 200 && resp.StatusCode < 300 &&
			cfg.blockRetry && isContentEndpoint(r.URL.Path) {
			blocked, derr := p.checkBlock(resp, r.URL.Path, cfg.blockRetryMode)
			if derr != nil {
				slog.Warn("block check failed, passing through", "model", model, "err", derr)
			} else if blocked {
				if blockRetries < cfg.maxBlockRetries {
					newBody, ok := appendContinueMessages(body, r.Header)
					if ok {
						blockRetries++
						body = newBody
						slog.Info("safety block detected, retrying with continue messages",
							"key", key.id, "model", model, "block_retry", blockRetries)
						last = resp
						continue
					}
					slog.Warn("block retry skipped: unable to rewrite request body", "model", model)
				} else {
					slog.Info("safety block persists after block retries exhausted, passing through",
						"model", model, "block_retries", blockRetries)
				}
			}
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			copyResponse(w, resp)
			return
		case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429:
			p.pool.MarkInvalid(key.id, resp.StatusCode)
			slog.Warn("key marked invalid", "key", key.id, "status", resp.StatusCode, "model", model)
			last = resp
			continue
		case resp.StatusCode == 429:
			kind := consumeAndClassify429(resp)
			p.pool.LockModel(key.id, model, kind)
			slog.Warn("key locked", "key", key.id, "model", model, "kind", kind, "path", r.URL.Path)
			if kind == LockTPM {
				// TPM：请求自身的 token 数已超限，换 Key 重试无济于事，
				// 直接透传 429 给客户端，不再消耗其他 Key。
				copyResponse(w, resp)
				return
			}
			last = resp
			continue
		case resp.StatusCode >= 500:
			slog.Warn("upstream 5xx, passing through", "key", key.id, "status", resp.StatusCode, "model", model)
			copyResponse(w, resp) // 5xx 不重试，直接透传错误
			return
		default: // 3xx 等：直接透传，不重试
			copyResponse(w, resp)
			return
		}
	}

	if last != nil {
		copyResponse(w, last) // 全部重试失败：原样透传最后一次上游响应
		return
	}
	http.Error(w, "no usable API key", http.StatusServiceUnavailable)
}

// forward 构造上游请求：注入池中 Key（覆盖/剥离客户端 key），请求头忠实复制。
func (p *Proxy) forward(ctx context.Context, method string, src *url.URL, body []byte, header http.Header, key string, cfg proxyConfig) (*http.Response, error) {
	u := *src
	p.upstreamMu.RLock()
	up := p.upstream
	p.upstreamMu.RUnlock()
	u.Scheme = up.Scheme
	u.Host = up.Host
	q := u.Query()
	q.Del("key")
	q.Set("key", key)
	u.RawQuery = q.Encode()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	req.Header.Del("x-goog-api-key") // 一律使用池中 Key
	req.Header.Del("Content-Length") // 由 Go 依据新 body 重新计算（拦截重试会改写 body 长度）
	if cfg.blockRetry && cfg.blockRetryMode == BlockRetryModeStream && isContentEndpoint(src.Path) {
		// stream 模式需要检查流式响应首块，剥掉 Accept-Encoding 让上游返回未压缩流，
		// 避免 gzip 无法按块判定；客户端同样能正常接收 identity 编码。
		req.Header.Del("Accept-Encoding")
	}
	p.clientMu.RLock()
	client := p.client
	p.clientMu.RUnlock()
	return client.Do(req)
}

// copyResponse 逐字节忠实透传：状态码与响应头逐字段原样复制
// （仅排除 hop-by-hop 头），body 直通 + 每块写入后 Flush，
// 上游怎么发来就怎么发出去，不做任何缓冲聚合或改写。
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				f.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
	resp.Body.Close()
}

func isHopByHop(name string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// requestAuthorized 校验客户端是否携带正确的网关访问密钥。
// 兼容 new-api Gemini 渠道的传参方式（x-goog-api-key 头）与通用 Bearer 方式。
// 注意：客户端自带的 key 在鉴权通过后仍会被 forward 剥离并替换为池中 Key。
func requestAuthorized(r *http.Request, password string) bool {
	if v := r.Header.Get("x-goog-api-key"); v != "" {
		return constantTimeEq(v, password)
	}
	const prefix = "Bearer "
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, prefix) {
		return constantTimeEq(strings.TrimSpace(strings.TrimPrefix(v, prefix)), password)
	}
	return false
}

// constantTimeEq 以恒定时间比较两个字符串，降低时序侧信道泄露风险。
func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// quotaViolation 是 Google rpc QuotaFailure 的 violation 条目。
type quotaViolation struct {
	QuotaID string `json:"quotaId"`
}

// 429 响应体的 details 条目结构。Gemini API 实测有两种形态：
//   - details[i].violations（@type=...QuotaFailure 直接挂 violations，实测格式）
//   - details[i].quotaFailure.violations（文档示例格式）
type quotaDetail struct {
	QuotaFailure struct {
		Violations []quotaViolation `json:"violations"`
	} `json:"quotaFailure"`
	Violations []quotaViolation `json:"violations"`
}

// 429 错误响应体（非流式端点）：{"error":{"details":[...]}}
type quotaErrPayload struct {
	Error struct {
		Details []quotaDetail `json:"details"`
	} `json:"error"`
}

// collectQuotaIDs 提取响应中所有 quotaId。
func collectQuotaIDs(details []quotaDetail) []string {
	var ids []string
	for _, d := range details {
		vs := d.Violations
		if len(vs) == 0 {
			vs = d.QuotaFailure.Violations
		}
		for _, v := range vs {
			ids = append(ids, v.QuotaID)
		}
	}
	return ids
}

// consumeAndClassify429 读取 429 响应体并判定限流类型：
//   - quotaId 含 "PerDay"           → RPD：锁到当日额度刷新点
//   - quotaId 含 "PerMinute"+"Tokens"（如 GenerateContentInputTokensPerModelPerMinute-*）
//     → TPM：锁 rpmLockDur，不换 Key 重试，直接透传
//   - quotaId 含 "PerMinute"（请求数超限） → RPM：锁 rpmLockDur 后换 Key 重试
//   - 解析不到 quotaId（未知/无法识别的 429）→ 兜底 TPM：只锁当前 Key×Model 并透传，
//     不逐一重试耗尽整个 Key 池，保证健壮性
//
// 注意响应体有几种形态，都要兼容：
//   - 非流式端点 generateContent：{"error":{"details":[...]}}
//   - 流式端点 streamGenerateContent：[{...}]（JSON 数组，无 alt=sse 时）
//   - 客户端带 Accept-Encoding: gzip 时上游返回 gzip 压缩字节（先解压再解析）
//
// resp.Body 读完重建为原始字节（保持 gzip 原样），供后续透传给客户端。
func consumeAndClassify429(resp *http.Response) LockKind {
	raw, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(raw))

	body := raw
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				body = plain
			}
			zr.Close()
		}
	}

	var ids []string
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []quotaErrPayload
		if json.Unmarshal(body, &arr) == nil {
			for _, p := range arr {
				ids = append(ids, collectQuotaIDs(p.Error.Details)...)
			}
		}
	} else {
		var obj quotaErrPayload
		if json.Unmarshal(body, &obj) == nil {
			ids = append(ids, collectQuotaIDs(obj.Error.Details)...)
		}
	}

	kind := LockTPM // 兜底：无法识别限流类型时按 TPM 处理，不重试耗尽 Key 池
	for _, id := range ids {
		switch {
		case strings.Contains(id, "PerDay"):
			kind = LockRPD
		case strings.Contains(id, "PerMinute") && strings.Contains(id, "Tokens"):
			kind = LockTPM
		case strings.Contains(id, "PerMinute"):
			kind = LockRPM
		}
	}
	return kind
}

// extractModel 从路径 /v1beta/models/{model}:generateContent 提取 model。
func extractModel(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s == "models" && i+1 < len(segs) {
			return strings.SplitN(segs[i+1], ":", 2)[0]
		}
	}
	return ""
}

// isContentEndpoint 判断路径是否为生成端点（只有这类请求的 body 里有 contents 可供追加）。
func isContentEndpoint(path string) bool {
	return strings.HasSuffix(path, ":generateContent") ||
		strings.HasSuffix(path, ":streamGenerateContent")
}

// readAndCheckBlock 读取响应体并判定是否被安全拦截，读完后重建 body 供后续透传使用。
func (p *Proxy) readAndCheckBlock(resp *http.Response) (bool, error) {
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	return responseBlocked(raw, resp.Header.Get("Content-Type")), nil
}

// checkBlock 按 blockRetryMode 选择拦截判定方式。
func (p *Proxy) checkBlock(resp *http.Response, path, mode string) (bool, error) {
	if mode == BlockRetryModeStream && strings.HasSuffix(path, ":streamGenerateContent") {
		return p.checkFirstChunkBlock(resp)
	}
	return p.readAndCheckBlock(resp)
}

// firstChunkMaxBytes 是 stream 模式下为判定拦截最多缓冲的首块字节数。
// 超过该上限仍无法判定时按未拦截透传，优先保证流式实时性。
const firstChunkMaxBytes = 64 * 1024

// readCloser 把重建后的多段 reader 与原响应 body 的 Close 绑定，保证上游连接最终被关闭。
type readCloser struct {
	io.Reader
	io.Closer
}

// checkFirstChunkBlock 只检查流式响应首块：SSE 首事件或 JSON 数组首元素。
// gzip 响应无法按块判定，回退到完整缓冲检查。
func (p *Proxy) checkFirstChunkBlock(resp *http.Response) (bool, error) {
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		return p.readAndCheckBlock(resp)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return p.checkFirstSSEEvent(resp)
	}
	return p.checkFirstJSONArrayElement(resp)
}

// checkFirstSSEEvent 读取首个 SSE 事件（到空行截止），仅对它判定是否被拦截；
// 无论结果如何都会把已读字节与剩余流重建回 resp.Body，未拦截时立即透传。
// 首事件超过 firstChunkMaxBytes 仍未结束时停止检查，按未拦截透传（截断的事件通常解析失败）。
func (p *Proxy) checkFirstSSEEvent(resp *http.Response) (bool, error) {
	br := bufio.NewReaderSize(resp.Body, 32*1024)
	var first bytes.Buffer
	for first.Len() < firstChunkMaxBytes {
		frag, err := br.ReadSlice('\n')
		first.Write(frag)
		if err == bufio.ErrBufferFull {
			continue // 单行超过缓冲区：继续读下一段，直到凑够上限或遇到换行
		}
		if err == io.EOF {
			break // 流已结束（不足一个完整事件）
		}
		if err != nil {
			resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(first.Bytes()), br), Closer: resp.Body}
			return false, err
		}
		if string(frag) == "\n" || string(frag) == "\r\n" {
			break // 空行 = 首事件结束
		}
	}
	resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(first.Bytes()), br), Closer: resp.Body}
	return sseBlocked(first.Bytes()), nil
}

// checkFirstJSONArrayElement 只检查 streamGenerateContent 无 alt=sse 时 JSON 数组流的
// 第一个元素。先用 TeeReader 记录 decoder 实际消费的字节，再原样重建 body，保证
// 透传字节级一致（decoder 的少量预读也会被完整回放）。
func (p *Proxy) checkFirstJSONArrayElement(resp *http.Response) (bool, error) {
	br := bufio.NewReaderSize(resp.Body, 32*1024)
	var before bytes.Buffer // 数组起始 '[' 之前的空白
	b, err := br.ReadByte()
	for err == nil && isSpaceByte(b) {
		before.WriteByte(b)
		b, err = br.ReadByte()
	}
	if err != nil {
		resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(before.Bytes()), br), Closer: resp.Body}
		return false, nil // 空流，未拦截
	}
	if b != '[' {
		// 不是数组流（例如非流式 JSON 对象），重建后回退完整缓冲检查。
		prefix := append(before.Bytes(), b)
		resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), br), Closer: resp.Body}
		return p.readAndCheckBlock(resp)
	}

	var afterOpen bytes.Buffer // '[' 与首元素之间的空白
	firstByte, err := br.ReadByte()
	for err == nil && isSpaceByte(firstByte) {
		afterOpen.WriteByte(firstByte)
		firstByte, err = br.ReadByte()
	}
	if err != nil {
		// 空数组 / 只有空白：无首元素可判，透传。
		prefix := append(append(before.Bytes(), '['), afterOpen.Bytes()...)
		resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), br), Closer: resp.Body}
		return false, nil
	}

	var consumed bytes.Buffer
	dec := json.NewDecoder(io.TeeReader(
		io.MultiReader(bytes.NewReader([]byte{firstByte}), br), &consumed))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		// 首元素不是合法 JSON：把 decoder 已读字节放回后透传。
		prefix := append(append(before.Bytes(), '['), afterOpen.Bytes()...)
		prefix = append(prefix, consumed.Bytes()...)
		resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), br), Closer: resp.Body}
		return false, nil
	}

	var obj genBlockResp
	blocked := json.Unmarshal(raw, &obj) == nil && obj.blocked()

	// 原样重建：'[' 之前的空白 + '[' + '[' 后空白 + decoder 已消费字节 + 剩余流。
	prefix := make([]byte, 0, before.Len()+1+afterOpen.Len()+consumed.Len())
	prefix = append(prefix, before.Bytes()...)
	prefix = append(prefix, '[')
	prefix = append(prefix, afterOpen.Bytes()...)
	prefix = append(prefix, consumed.Bytes()...)
	resp.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), br), Closer: resp.Body}
	return blocked, nil
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// genBlockResp 是 GenerateContentResponse 中与安全拦截判定相关的字段。
type genBlockResp struct {
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Candidates []struct {
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
}

// blocked 报告该响应是否被安全机制拦截。
func (g genBlockResp) blocked() bool {
	if g.PromptFeedback.BlockReason != "" {
		return true
	}
	for _, c := range g.Candidates {
		switch c.FinishReason {
		case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
			return true
		}
	}
	return false
}

// responseBlocked 判断响应体是否包含安全拦截信号，兼容多种形态：
//   - 非流式 generateContent：{"promptFeedback":{"blockReason":...}}
//   - 流式 streamGenerateContent（alt=sse）：data: {...}\n\n
//   - 流式无 alt=sse：JSON 数组 [{...}]
//   - gzip 压缩字节（客户端带 Accept-Encoding: gzip 时）
func responseBlocked(raw []byte, contentType string) bool {
	body := raw
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				body = plain
			}
			zr.Close()
		}
	}

	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return false
	}
	if strings.Contains(contentType, "text/event-stream") || bytes.HasPrefix(trimmed, []byte("data:")) {
		return sseBlocked(body)
	}
	if trimmed[0] == '[' {
		var arr []genBlockResp
		if json.Unmarshal(body, &arr) == nil {
			for i := range arr {
				if arr[i].blocked() {
					return true
				}
			}
		}
		return false
	}
	var obj genBlockResp
	if json.Unmarshal(body, &obj) == nil {
		return obj.blocked()
	}
	return false
}

// sseBlocked 解析 SSE 事件流中的 data: JSON，逐条判定是否被拦截。
func sseBlocked(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj genBlockResp
		if json.Unmarshal(payload, &obj) == nil && obj.blocked() {
			return true
		}
	}
	return false
}

// appendContinueMessages 在请求体 contents 末尾追加一条 user 消息，用于安全拦截自动重试：
//   - {"role":"user","parts":[{"text":"EOF"}]}
//
// 仅处理 JSON 请求体；若请求体为 gzip（Content-Encoding: gzip 或魔数），先解压改写再重新压缩。
// 任一步失败返回 ok=false，调用方应放弃重试并原样透传。
func appendContinueMessages(body []byte, header http.Header) ([]byte, bool) {
	gzipped := strings.EqualFold(header.Get("Content-Encoding"), "gzip") ||
		(len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b)
	plain := body
	if gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, false
		}
		plain, err = io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return body, false
		}
	}

	var m map[string]any
	if err := json.Unmarshal(plain, &m); err != nil {
		return body, false
	}
	contents, _ := m["contents"].([]any)
	contents = append(contents,
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "EOF"}}},
	)
	m["contents"] = contents
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 关闭 HTML 转义，保留追加消息字面量
	if err := enc.Encode(m); err != nil {
		return body, false
	}
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if gzipped {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(out); err != nil {
			return body, false
		}
		if err := zw.Close(); err != nil {
			return body, false
		}
		out = buf.Bytes()
	}
	return out, true
}
