package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestProxy 搭建完整链路：gateway(proxy) → mock 上游。
func newTestProxy(t *testing.T, keys []string, upstreamHandler http.HandlerFunc) (*Pool, *httptest.Server) {
	t.Helper()
	return newTestProxyT(t, keys, 5*time.Second, upstreamHandler)
}

func newTestProxyT(t *testing.T, keys []string, requestTimeout time.Duration, upstreamHandler http.HandlerFunc) (*Pool, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	pool := NewPool(keys)
	proxy := NewProxy(pool, up.URL, 5, requestTimeout)
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	return pool, gate
}

// newBlockRetryProxy 搭建启用安全拦截自动重试的链路：gateway(proxy) → mock 上游。
func newBlockRetryProxy(t *testing.T, keys []string, maxBlockRetries int, upstreamHandler http.HandlerFunc) (*Pool, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	pool := NewPool(keys)
	proxy := NewProxy(pool, up.URL, 5, 5*time.Second)
	proxy.SetBlockRetry(true, maxBlockRetries)
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	return pool, gate
}

// newBlockRetryStreamProxy 搭建启用 stream 模式安全拦截自动重试的链路。
func newBlockRetryStreamProxy(t *testing.T, keys []string, maxBlockRetries int, upstreamHandler http.HandlerFunc) (*Pool, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	pool := NewPool(keys)
	proxy := NewProxy(pool, up.URL, 5, 5*time.Second)
	proxy.SetBlockRetry(true, maxBlockRetries, BlockRetryModeStream)
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	return pool, gate
}

func post(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func byKey(key string) string { return strings.Split(key, "=")[1] }

// keyInQuery 从 mock 侧读上游收到的 key 参数。
func keyInQuery(r *http.Request) string { return r.URL.Query().Get("key") }

func Test4xxMarkInvalidAndSwitch(t *testing.T) {
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		if keyInQuery(r) == "k1" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (switched to k2)", resp.StatusCode)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", raw, `{"ok":true}`)
	}
	snap := pool.Snapshot()
	if snap.Keys[0].State != "invalid" {
		t.Errorf("k1 state = %s, want invalid", snap.Keys[0].State)
	}
	if snap.Keys[0].Failures != 1 || snap.Keys[1].Requests != 1 {
		t.Errorf("unexpected counters: k1 failures=%d k2 requests=%d", snap.Keys[0].Failures, snap.Keys[1].Requests)
	}
}

func Test429RPDLockAndSwitch(t *testing.T) {
	rpdBody := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","quotaFailure":{"violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests","quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaDimensions":{"model":"gemini-2.0-flash","location":"global"},"quotaValue":"20"}]}}]}}`
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		if keyInQuery(r) == "k1" {
			w.WriteHeader(429)
			w.Write([]byte(rpdBody))
			return
		}
		w.Write([]byte("ok"))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	if resp.StatusCode != 200 || string(raw) != "ok" {
		t.Fatalf("status=%d body=%q, want 200 ok", resp.StatusCode, raw)
	}
	snap := pool.Snapshot()
	lock, ok := snap.Keys[0].Locks["gemini-2.0-flash"]
	if !ok || lock.Kind != string(LockRPD) {
		t.Fatalf("k1 should be RPD-locked on model, got %+v", snap.Keys[0].Locks)
	}
	// RPD 锁只针对该 model
	if k := pool.Pick("gemini-3.0-flash"); k == nil || k.id != shortID("k1") {
		t.Error("k1 should be usable on other models")
	}
}

func Test429RPMShortCooldown(t *testing.T) {
	rpmBody := `{"error":{"code":429,"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","quotaFailure":{"violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier"}]}}]}}`
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		if keyInQuery(r) == "k1" {
			w.WriteHeader(429)
			w.Write([]byte(rpmBody))
			return
		}
		w.Write([]byte("ok"))
	})

	post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	snap := pool.Snapshot()
	lock, ok := snap.Keys[0].Locks["gemini-2.0-flash"]
	if !ok || lock.Kind != string(LockRPM) {
		t.Fatalf("k1 should be RPM-locked, got %+v", snap.Keys[0].Locks)
	}
	if until := lock.Until.Sub(time.Now()); until > 65*time.Second || until < 55*time.Second {
		t.Errorf("RPM lock duration = %v, want ~60s", until)
	}
}

func TestClassify429(t *testing.T) {
	cases := []struct {
		name    string
		quotaID string
		want    LockKind
		flat    bool // true: violations 直接挂 details[i]（实测格式）；false: 嵌套 quotaFailure（文档格式）
		array   bool // true: 流式端点格式，body 为 JSON 数组 [{...}]
	}{
		{"rpd", "GenerateRequestsPerDayPerProjectPerModel-FreeTier", LockRPD, false, false},
		{"rpd-input-tokens", "GenerateContentInputTokensPerDayFreeTier", LockRPD, false, false},
		{"rpm", "GenerateRequestsPerMinutePerProjectPerModel-FreeTier", LockRPM, false, false},
		{"tpm-input", "GenerateContentInputTokensPerModelPerMinute-FreeTier", LockTPM, false, false},
		{"tpm-output", "GenerateContentOutputTokensPerMinutePerProjectPerModel", LockTPM, false, false},
		{"tpm-flat", "GenerateContentInputTokensPerModelPerMinute-FreeTier", LockTPM, true, false},
		{"rpm-flat", "GenerateRequestsPerMinutePerProjectPerModel-FreeTier", LockRPM, true, false},
		{"rpd-flat", "GenerateRequestsPerDayPerProjectPerModel-FreeTier", LockRPD, true, false},
		{"tpm-stream-array", "GenerateContentInputTokensPerModelPerMinute-FreeTier", LockTPM, true, true},
		{"rpm-stream-array", "GenerateRequestsPerMinutePerProjectPerModel-FreeTier", LockRPM, true, true},
		{"unknown-quota-id", "SomeFutureQuotaNamePerHour", LockTPM, true, false}, // 无法识别的 quotaId → 兜底 TPM
		{"no-quota-details", "", LockTPM, false, false},                          // 解析不到 quotaId → 兜底 TPM
	}
	for _, c := range cases {
		var body string
		if c.quotaID == "" {
			body = `{"error":{"code":429}}`
		} else if c.flat {
			// 实测 Gemini API 格式：@type 标记 + violations 直接挂 details[i]
			body = fmt.Sprintf(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":%q}]}]}}`, c.quotaID)
		} else {
			body = fmt.Sprintf(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","quotaFailure":{"violations":[{"quotaId":%q}]}}]}}`, c.quotaID)
		}
		if c.array {
			// 流式端点（streamGenerateContent）429 响应为 JSON 数组
			body = "[" + body + "]"
		}
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
		if got := consumeAndClassify429(resp); got != c.want {
			t.Errorf("%s: classify = %s, want %s", c.name, got, c.want)
		}
		// body 必须被重建，供后续透传使用
		rebuilt, _ := io.ReadAll(resp.Body)
		if string(rebuilt) != body {
			t.Errorf("%s: body not restored for passthrough", c.name)
		}
	}
}

func TestClassify429Gzip(t *testing.T) {
	// 客户端带 Accept-Encoding: gzip 时，上游 429 响应体为 gzip 压缩字节
	plain := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateContentInputTokensPerModelPerMinute-FreeTier"}]}]}}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(plain))
	zw.Close()
	gz := buf.Bytes()

	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(gz))}
	if got := consumeAndClassify429(resp); got != LockTPM {
		t.Errorf("gzip body classify = %s, want %s", got, LockTPM)
	}
	// 透传必须保持 gzip 原样
	rebuilt, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(rebuilt, gz) {
		t.Errorf("gzip body not preserved for passthrough")
	}

	// gzip 数组格式（流式端点 + gzip 头）
	var bufArr bytes.Buffer
	zwArr := gzip.NewWriter(&bufArr)
	zwArr.Write([]byte(`[` + plain + `]`))
	zwArr.Close()
	gzArr := bufArr.Bytes()
	resp2 := &http.Response{Body: io.NopCloser(bytes.NewReader(gzArr))}
	if got := consumeAndClassify429(resp2); got != LockTPM {
		t.Errorf("gzip array body classify = %s, want %s", got, LockTPM)
	}
	rebuilt2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(rebuilt2, gzArr) {
		t.Errorf("gzip array body not preserved for passthrough")
	}
}

func Test429TPMLockAndPassthroughNoRetry(t *testing.T) {
	// 实测 Gemini API 429 响应格式：violations 直接挂 details[i]（@type 标记），无 quotaFailure 嵌套
	tpmBody := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_input_token_count, limit: 250000","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateContentInputTokensPerModelPerMinute-FreeTier","quotaValue":"250000"}]}]}}`
	var hits int
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(429)
		w.Write([]byte(tpmBody))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 (exact passthrough)", resp.StatusCode)
	}
	if string(raw) != tpmBody {
		t.Errorf("body = %q, want exact upstream TPM body", raw)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (TPM must NOT retry other keys)", hits)
	}
	snap := pool.Snapshot()
	lock, ok := snap.Keys[0].Locks["gemini-2.0-flash"]
	if !ok || lock.Kind != string(LockTPM) {
		t.Fatalf("k1 should be TPM-locked, got %+v", snap.Keys[0].Locks)
	}
	if until := lock.Until.Sub(time.Now()); until > 65*time.Second || until < 55*time.Second {
		t.Errorf("TPM lock duration = %v, want ~60s", until)
	}
	if len(snap.Keys[1].Locks) != 0 || snap.Keys[1].Requests != 0 {
		t.Errorf("k2 must be untouched: locks=%v requests=%d", snap.Keys[1].Locks, snap.Keys[1].Requests)
	}
}

func Test429TPMStreamArrayLockAndPassthroughNoRetry(t *testing.T) {
	// 流式端点（streamGenerateContent）实测：429 响应为 JSON 数组 [{...}]
	tpmBody := `[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_input_token_count, limit: 250000","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateContentInputTokensPerModelPerMinute-FreeTier","quotaValue":"250000"}]}]}}]`
	var hits int
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(429)
		w.Write([]byte(tpmBody))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent", `{}`)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 (exact passthrough)", resp.StatusCode)
	}
	if string(raw) != tpmBody {
		t.Errorf("body = %q, want exact upstream TPM body", raw)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (stream-array TPM must NOT retry other keys)", hits)
	}
	snap := pool.Snapshot()
	lock, ok := snap.Keys[0].Locks["gemini-2.0-flash"]
	if !ok || lock.Kind != string(LockTPM) {
		t.Fatalf("k1 should be TPM-locked, got %+v", snap.Keys[0].Locks)
	}
	if len(snap.Keys[1].Locks) != 0 || snap.Keys[1].Requests != 0 {
		t.Errorf("k2 must be untouched: locks=%v requests=%d", snap.Keys[1].Locks, snap.Keys[1].Requests)
	}
}

func Test5xxPassThroughNoRetry(t *testing.T) {
	var hits int
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("X-Upstream-Probe", "p5xx")
		w.WriteHeader(500)
		w.Write([]byte("upstream boom"))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	if resp.StatusCode != 500 || string(raw) != "upstream boom" {
		t.Fatalf("status=%d body=%q, want 500 upstream boom (exact passthrough)", resp.StatusCode, raw)
	}
	if resp.Header.Get("X-Upstream-Probe") != "p5xx" {
		t.Error("5xx upstream headers not passed through")
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (5xx must NOT retry)", hits)
	}
	snap := pool.Snapshot()
	if snap.Keys[0].State != "available" || snap.Keys[1].State != "available" {
		t.Error("5xx must not mark keys invalid")
	}
}

func TestAllRetriesFailPassthroughLast(t *testing.T) {
	pool, gate := newTestProxy(t, []string{"k1", "k2"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"code":400,"message":"last error body"}}`))
	})
	// maxRetries=5, 2 keys → 最多 6 次尝试，全部 400
	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 (last upstream response)", resp.StatusCode)
	}
	if string(raw) != `{"error":{"code":400,"message":"last error body"}}` {
		t.Errorf("body = %q, want exact last upstream body", raw)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Error("upstream header X-Upstream not passed through")
	}
	if snap := pool.Snapshot(); snap.Keys[0].State != "invalid" || snap.Keys[1].State != "invalid" {
		t.Error("both keys should be invalid after all 4xx")
	}
}

func TestClientKeyStripped(t *testing.T) {
	var receivedKey string
	pool, gate := newTestProxy(t, []string{"realkey"}, func(w http.ResponseWriter, r *http.Request) {
		receivedKey = keyInQuery(r)
		if r.URL.Query().Get("alt") != "sse" {
			t.Error("other query params must be preserved")
		}
		w.Write([]byte("ok"))
	})
	post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent?key=evil&alt=sse", `{}`)
	if receivedKey != "realkey" {
		t.Errorf("upstream received key=%q, want realkey", receivedKey)
	}
	if snap := pool.Snapshot(); snap.Keys[0].Requests != 1 {
		t.Errorf("requests = %d, want 1", snap.Keys[0].Requests)
	}
	_ = pool
}

func TestSSEFaithfulStreaming(t *testing.T) {
	events := []string{
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"}]}}]}\n\n",
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"世界\"}]}}]}\n\n",
		"data: [DONE]\n\n",
	}
	pool, gate := newTestProxy(t, []string{"k1"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl := w.(http.Flusher)
		for _, e := range events {
			fmt.Fprint(w, e)
			fl.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})

	resp, err := http.Get(gate.URL + "/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 读第一个 chunk：若网关有缓冲聚合，Read 会阻塞到上游全部写完（~150ms）
	start := time.Now()
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no data in first read")
	}
	firstArrival := time.Since(start)
	if firstArrival > 120*time.Millisecond {
		t.Errorf("first SSE chunk arrived after %v (gateway buffering?)", firstArrival)
	}

	var raw bytes.Buffer
	raw.Write(buf[:n])
	rest, _ := io.ReadAll(resp.Body)
	raw.Write(rest)

	want := strings.Join(events, "")
	if raw.String() != want {
		t.Errorf("SSE body mismatch:\n got %q\nwant %q", raw.String(), want)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if pool.Snapshot().Keys[0].Requests != 1 {
		t.Error("requests should be 1")
	}
}

func TestConcurrentRequests(t *testing.T) {
	pool, gate := newTestProxy(t, []string{"k1", "k2", "k3"}, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.Write([]byte("ok"))
	})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	snap := pool.Snapshot()
	if snap.Keys[0].Requests+snap.Keys[1].Requests+snap.Keys[2].Requests != 100 {
		t.Errorf("total requests = %d, want 100", snap.Keys[0].Requests+snap.Keys[1].Requests+snap.Keys[2].Requests)
	}
}

func TestNoUsableKeyReturns503(t *testing.T) {
	_, gate := newTestProxy(t, []string{"k1"}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte("bad"))
	})
	// 两个 key 都 400 → 之后无可用 key → 第三次请求应 503
	for i := 0; i < 2; i++ {
		post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	}
	resp, _ := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestUpstreamTimeoutReturns503NoRetry(t *testing.T) {
	var hits atomic.Int32
	pool, gate := newTestProxyT(t, []string{"k1", "k2"}, 100*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond) // 挂起：不发任何响应头
		w.Write([]byte("late"))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if string(raw) != "The Gemini API did not provide any response before timing out.\n" {
		t.Errorf("body = %q, want gateway timeout message", raw)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (timeout must NOT retry)", n)
	}
	snap := pool.Snapshot()
	if snap.Keys[0].State != "available" || snap.Keys[1].State != "available" {
		t.Error("timeout must not mark keys invalid")
	}
	if snap.Keys[0].Failures != 1 || snap.Keys[1].Failures != 0 {
		t.Errorf("unexpected failures: k1=%d k2=%d", snap.Keys[0].Failures, snap.Keys[1].Failures)
	}
}

// --- 安全拦截自动重试 ---

func TestBlockRetryAppendsContinueAndSucceeds(t *testing.T) {
	var bodies []string
	var keys []string
	_, gate := newBlockRetryProxy(t, []string{"k1", "k2"}, 1, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		keys = append(keys, keyInQuery(r))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
			return
		}
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"继续吧"}]}}]}`))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{"contents":[{"role":"user","parts":[{"text":"早上好"}]}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(raw) != `{"candidates":[{"content":{"parts":[{"text":"继续吧"}]}}]}` {
		t.Errorf("body = %q, want retried success body", raw)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream hits = %d, want 2 (1 blocked + 1 retry)", len(bodies))
	}
	if keys[0] != keys[1] {
		t.Errorf("block retry should reuse the same key, got %q then %q", keys[0], keys[1])
	}
	for _, want := range []string{"System:网络错误", "卡了，继续", `"role":"model"`, `"role":"user"`} {
		if !strings.Contains(bodies[1], want) {
			t.Errorf("retried body missing %q: %s", want, bodies[1])
		}
	}
}

func TestBlockRetryDisabledPassthrough(t *testing.T) {
	var hits int
	_, gate := newTestProxy(t, []string{"k1"}, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (block passed through)", resp.StatusCode)
	}
	if string(raw) != `{"promptFeedback":{"blockReason":"SAFETY"}}` {
		t.Errorf("body = %q, want exact upstream block body", raw)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (block retry disabled)", hits)
	}
}

func TestBlockRetryStreaming(t *testing.T) {
	var hits int
	var lastBody string
	_, gate := newBlockRetryProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		raw, _ := io.ReadAll(r.Body)
		lastBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if hits == 1 {
			fmt.Fprint(w, "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}
	if !strings.Contains(string(raw), `"text":"hi"`) {
		t.Errorf("retried SSE body = %q, want candidate text", raw)
	}
	if !strings.Contains(lastBody, "卡了，继续") {
		t.Errorf("retried stream request body missing continue message: %s", lastBody)
	}
}

func TestBlockRetryFullModeRetriedResponseStillBuffered(t *testing.T) {
	firstSent := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer doRelease()

	var hits int
	_, gate := newBlockRetryProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if hits == 1 {
			fmt.Fprint(w, "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")
		fl.Flush()
		close(firstSent)
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	type getResult struct {
		resp *http.Response
		err  error
	}
	got := make(chan getResult, 1)
	go func() {
		resp, err := http.Post(gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse",
			"application/json", strings.NewReader(`{"contents":[]}`))
		got <- getResult{resp, err}
	}()

	select {
	case <-firstSent:
	case <-time.After(time.Second):
		t.Fatal("upstream retry first event not sent")
	}

	// full 模式重试后的最终响应也必须完整缓冲：客户端不应在上游结束前收到任何字节
	select {
	case g := <-got:
		if g.err == nil {
			g.resp.Body.Close()
		}
		t.Fatalf("full mode must NOT stream retried response before upstream completes, got resp=%v err=%v", g.resp, g.err)
	case <-time.After(150 * time.Millisecond):
	}

	doRelease()
	select {
	case g := <-got:
		if g.err != nil {
			t.Fatalf("get after upstream completed: %v", g.err)
		}
		defer g.resp.Body.Close()
		raw, _ := io.ReadAll(g.resp.Body)
		for _, want := range []string{"hi", "[DONE]"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("body = %q, want %q", raw, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("no response after upstream completed")
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2", hits)
	}
}

func TestBlockRetryStreamModeSSEInitialBlockRetries(t *testing.T) {
	var hits int
	var lastBody string
	_, gate := newBlockRetryStreamProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		raw, _ := io.ReadAll(r.Body)
		lastBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if hits == 1 {
			fmt.Fprint(w, "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (initial SSE block must retry)", hits)
	}
	if !strings.Contains(string(raw), `"text":"hi"`) {
		t.Errorf("retried SSE body = %q, want candidate text", raw)
	}
	if !strings.Contains(lastBody, "卡了，继续") {
		t.Errorf("retried stream request body missing continue message: %s", lastBody)
	}
}

func TestBlockRetryStreamModeOnlyFirstChunkChecked(t *testing.T) {
	var hits int
	_, gate := newBlockRetryStreamProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (mid-stream block must NOT retry in stream mode)", hits)
	}
	for _, want := range []string{"你好", "SAFETY"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("passthrough body missing %q: %s", want, raw)
		}
	}
}

func TestBlockRetryStreamModeKeepsStreaming(t *testing.T) {
	firstSent := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer doRelease()

	_, gate := newBlockRetryStreamProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"}]}}]}\n\n")
		fl.Flush()
		close(firstSent)
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	resp, err := http.Get(gate.URL + "/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	type readResult struct {
		n   int
		err error
	}
	got := make(chan readResult, 1)
	go func() {
		n, err := resp.Body.Read(buf)
		got <- readResult{n, err}
	}()

	var first readResult
	select {
	case first = <-got:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first SSE chunk not received while upstream still streaming (gateway buffering?)")
	}
	if first.err != nil && first.err != io.EOF {
		t.Fatalf("first read error: %v", first.err)
	}
	if first.n == 0 {
		t.Fatal("first read returned 0 bytes")
	}
	if !strings.Contains(string(buf[:first.n]), "你好") {
		t.Errorf("first SSE chunk = %q, want candidate text", buf[:first.n])
	}
	select {
	case <-firstSent:
	default:
		t.Error("upstream first event not flushed yet")
	}

	doRelease()
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("rest body = %q, want [DONE]", rest)
	}
}

func TestBlockRetryStreamModeJSONArrayInitialBlockRetries(t *testing.T) {
	var hits int
	var lastBody string
	_, gate := newBlockRetryStreamProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		raw, _ := io.ReadAll(r.Body)
		lastBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		if hits == 1 {
			fmt.Fprint(w, `[{"promptFeedback":{"blockReason":"SAFETY"}}]`)
			return
		}
		fmt.Fprint(w, `[{"candidates":[{"content":{"parts":[{"text":"继续吧"}]}}]}]`)
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"早上好"}]}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (initial JSON array block must retry)", hits)
	}
	if !strings.Contains(string(raw), "继续吧") {
		t.Errorf("retried JSON array body = %q, want candidate text", raw)
	}
	if !strings.Contains(lastBody, "卡了，继续") {
		t.Errorf("retried stream request body missing continue message: %s", lastBody)
	}
}

func TestBlockRetryStreamModeJSONArrayPassthrough(t *testing.T) {
	body := `[{"candidates":[{"content":{"parts":[{"text":"你好"}]}}]},{"promptFeedback":{"blockReason":"SAFETY"}}]`
	var hits int
	_, gate := newBlockRetryStreamProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:streamGenerateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (first element normal must NOT retry)", hits)
	}
	if string(raw) != body {
		t.Errorf("body = %q, want exact upstream body %q", raw, body)
	}
}

func TestBlockRetryExhaustedPassthrough(t *testing.T) {
	var hits int
	_, gate := newBlockRetryProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	})

	resp, raw := post(t, gate.URL+"/v1beta/models/gemini-2.0-flash:generateContent", `{"contents":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(raw) != `{"promptFeedback":{"blockReason":"SAFETY"}}` {
		t.Errorf("body = %q, want still-blocked upstream body", raw)
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (1 original + 1 retry then passthrough)", hits)
	}
}

func TestBlockRetryNonContentEndpointUnaffected(t *testing.T) {
	var hits int
	_, gate := newBlockRetryProxy(t, []string{"k1"}, 1, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	})

	// models 列表端点没有 contents，不应触发拦截重试
	resp, raw := post(t, gate.URL+"/v1beta/models", `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(raw) != `{"promptFeedback":{"blockReason":"SAFETY"}}` {
		t.Errorf("body = %q, want passthrough", raw)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (no block retry on non-content endpoint)", hits)
	}
}

func TestResponseBlockedDetection(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string
		want        bool
	}{
		{"obj-blocked", `{"promptFeedback":{"blockReason":"SAFETY"}}`, "application/json", true},
		{"obj-candidate-safety", `{"candidates":[{"finishReason":"SAFETY"}]}`, "application/json", true},
		{"obj-normal", `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, "application/json", false},
		{"array-blocked", `[{"promptFeedback":{"blockReason":"SAFETY"}}]`, "application/json", true},
		{"sse-blocked", "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n", "text/event-stream", true},
		{"sse-normal", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\ndata: [DONE]\n\n", "text/event-stream", false},
		{"empty", "", "application/json", false},
	}
	for _, c := range cases {
		if got := responseBlocked([]byte(c.body), c.contentType); got != c.want {
			t.Errorf("%s: blocked = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestResponseBlockedGzip(t *testing.T) {
	plain := `{"promptFeedback":{"blockReason":"SAFETY"}}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(plain))
	zw.Close()
	if !responseBlocked(buf.Bytes(), "application/json") {
		t.Error("gzip blocked body not detected")
	}
}

func TestAppendContinueMessages(t *testing.T) {
	in := `{"contents":[{"role":"user","parts":[{"text":"早上好"}]}],"systemInstruction":{"parts":[{"text":"你是助手"}]}}`
	out, ok := appendContinueMessages([]byte(in), http.Header{})
	if !ok {
		t.Fatal("appendContinueMessages returned ok=false")
	}
	for _, want := range []string{"System:网络错误", "卡了，继续", "你是助手"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if _, ok := m["systemInstruction"]; !ok {
		t.Error("systemInstruction field dropped")
	}
	contents, ok := m["contents"].([]any)
	if !ok || len(contents) != 3 {
		t.Fatalf("contents = %#v, want 3 entries (original + model + user)", m["contents"])
	}
}

func TestAppendContinueMessagesGzip(t *testing.T) {
	plain := `{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(plain))
	zw.Close()

	out, ok := appendContinueMessages(buf.Bytes(), http.Header{"Content-Encoding": []string{"gzip"}})
	if !ok {
		t.Fatal("appendContinueMessages gzip returned ok=false")
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output not gzip-encoded: %v", err)
	}
	decoded, _ := io.ReadAll(zr)
	zr.Close()
	if !strings.Contains(string(decoded), "卡了，继续") {
		t.Errorf("gzip roundtrip missing continue message: %s", decoded)
	}
}

func TestAppendContinueMessagesNonJSON(t *testing.T) {
	if _, ok := appendContinueMessages([]byte("not-json"), http.Header{}); ok {
		t.Error("appendContinueMessages should return ok=false for non-JSON body")
	}
}
