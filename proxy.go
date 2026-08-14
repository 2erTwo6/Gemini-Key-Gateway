package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
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
	pool       *Pool
	client     *http.Client
	upstream   *url.URL
	maxRetries int
}

func NewProxy(pool *Pool, upstream string, maxRetries int, requestTimeout time.Duration) *Proxy {
	u, err := url.Parse(upstream)
	if err != nil {
		u, _ = url.Parse(defaultUpstream)
	}
	tr := &http.Transport{
		DisableCompression:    true, // 不干预编码，保证透传忠实
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
	}
	return &Proxy{
		pool:       pool,
		client:     &http.Client{Transport: tr},
		upstream:   u,
		maxRetries: maxRetries,
	}
}

// ServeHTTP 转发到上游：轮询可用 Key、按响应分类重试、最后透传。
// 请求体读入内存（重试需重放）；响应流式透传，零缓冲改写。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	model := extractModel(r.URL.Path)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("read request body", "err", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var last *http.Response
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		key := p.pool.Pick(model)
		if key == nil {
			slog.Warn("no usable key", "model", model, "attempt", attempt)
			break
		}
		resp, err := p.forward(r.Context(), r.Method, r.URL, body, r.Header, key.key)
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
			slog.Warn("key locked", "key", key.id, "model", model, "kind", kind)
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
func (p *Proxy) forward(ctx context.Context, method string, src *url.URL, body []byte, header http.Header, key string) (*http.Response, error) {
	u := *src
	u.Scheme = p.upstream.Scheme
	u.Host = p.upstream.Host
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
	return p.client.Do(req)
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

// consumeAndClassify429 读取 429 响应体并判定限流类型：
//   - quotaId 含 "PerDay"           → RPD：锁到当日额度刷新点
//   - quotaId 含 "PerMinute"+"Tokens"（如 GenerateContentInputTokensPerModelPerMinute-*）
//     → TPM：锁 rpmLockDur，不换 Key 重试，直接透传
//   - 其余（请求数超限等）            → RPM：锁 rpmLockDur 后换 Key 重试
//
// 读完 body 后重建以便后续透传。
func consumeAndClassify429(resp *http.Response) LockKind {
	body, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(body))

	var payload struct {
		Error struct {
			Details []struct {
				QuotaFailure struct {
					Violations []struct {
						QuotaID string `json:"quotaId"`
					} `json:"violations"`
				} `json:"quotaFailure"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		for _, d := range payload.Error.Details {
			for _, v := range d.QuotaFailure.Violations {
				switch {
				case strings.Contains(v.QuotaID, "PerDay"):
					return LockRPD
				case strings.Contains(v.QuotaID, "PerMinute") && strings.Contains(v.QuotaID, "Tokens"):
					return LockTPM
				}
			}
		}
	}
	return LockRPM
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
