package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// countingUpstream 记录被调用次数：门锁命中时它必须保持 0。
func countingUpstream(t *testing.T, calls *int32) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt32(calls, 1)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

const latchReplyFixture = "冷咖啡已激活\n\nQQ一群 1057540028\n"

func latchFixture() *prompt.Latch {
	return &prompt.Latch{
		Words: []string{"冷咖啡", "cold coffee"},
		Reply: latchReplyFixture,
	}
}

// TestLatchShortCircuitsBeforeUpstream 命中门锁：200、正文逐字、上游零调用。
func TestLatchShortCircuitsBeforeUpstream(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls), PersonaLatch: latchFixture()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[{"role":"user","content":"冷咖啡"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("门锁命中不应打上游，实际调用 %d 次", got)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != latchReplyFixture {
		t.Errorf("激活页应逐字返回，got %+v", resp.Choices)
	}
}

// TestLatchMissFallsThrough 未命中：正常走上游。
func TestLatchMissFallsThrough(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls), PersonaLatch: latchFixture()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[{"role":"user","content":"冷咖啡不是激活词"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got == 0 {
		t.Error("未命中应正常打上游")
	}
}

// TestLatchNilDisabled 无门锁：即使输入恰好等于激活词也正常走上游。
func TestLatchNilDisabled(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls)}) // 无 PersonaLatch

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[{"role":"user","content":"冷咖啡"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("无门锁时不应短路")
	}
}

// TestLatchStreamSSE 流式命中：输出 SSE 帧，含完整正文与 [DONE]。
func TestLatchStreamSSE(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls), PersonaLatch: latchFixture()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","stream":true,"messages":[{"role":"user","content":"冷咖啡"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("流式命中也不应打上游")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("流式输出不成形: %q", body)
	}
	if !strings.Contains(body, "冷咖啡已激活") {
		t.Error("流式正文缺失")
	}
}

// TestLatchResponsesProtocol /v1/responses 命中：返回 Responses 形状（非 chat 形状）。
func TestLatchResponsesProtocol(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls), PersonaLatch: latchFixture()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"wb:glm-5.2","input":"冷咖啡"}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("responses 命中也不应打上游")
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if obj["object"] != "response" {
		t.Errorf("应为 Responses 形状，got object=%v", obj["object"])
	}
	if !strings.Contains(rec.Body.String(), "冷咖啡已激活") {
		t.Errorf("激活页缺失: %s", rec.Body)
	}
}

// TestLatchLastUserMessageOnly 只认最后一条 user：前面出现过激活词不影响判定。
func TestLatchLastUserMessageOnly(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls), PersonaLatch: latchFixture()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"wb:glm-5.2",
		"messages":[
			{"role":"user","content":"冷咖啡"},
			{"role":"assistant","content":"冷咖啡已激活"},
			{"role":"user","content":"继续干正事"}
		]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("最后一条 user 不是激活词，不应短路")
	}
}

// TestLatchTrimsWhitespace 前后空白不影响命中。
func TestLatchTrimsWhitespace(t *testing.T) {
	var calls int32
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: countingUpstream(t, &calls), PersonaLatch: latchFixture()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[{"role":"user","content":"  冷咖啡\n"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("去空白后相等应命中")
	}
}
