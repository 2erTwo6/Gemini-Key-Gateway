package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestProxy 搭建完整链路：gateway(proxy) → mock 上游。
func newTestProxy(t *testing.T, keys []string, upstreamHandler http.HandlerFunc) (*Pool, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)
	pool := NewPool(keys)
	proxy := NewProxy(pool, up.URL, 5)
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
