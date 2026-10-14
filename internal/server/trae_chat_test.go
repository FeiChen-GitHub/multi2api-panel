package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 一段最简 SOLO 流：一条 output + done。
const traeSOLOFixture = "event:output\ndata:{\"response\":\"Hello world\"}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// traeTestClient 构造一个 URL/host 全部指向 fake transport 的 TraeClient。
func traeTestClient(t *testing.T, calls *int) *upstream.TraeClient {
	t.Helper()
	tc := upstream.NewTraeClient()
	tc.AgentHost = "https://trae.fake"
	tc.UgHost = "https://trae.fake"
	tc.OAuthHost = "https://trae.fake"
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(traeSOLOFixture)),
		}, nil
	})}
	tc.HTTP = fake
	tc.StreamHTTP = fake
	return tc
}

// trae: 前缀的请求必须路由到 TraeClient，且绝不落到 WorkBuddy 客户端。
func TestChatRoutesTraeToTraeClient(t *testing.T) {
	wbCalls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		wbCalls++
		return 200, sseOK, true
	})
	traeCalls := 0
	tc := traeTestClient(t, &traeCalls)

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	wbAuth := &auth.Auth{UID: "wb1", AccessToken: "wb-at", ExpiresAt: 9999999999}
	p := testPoolWith(wbAuth, traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if traeCalls != 1 {
		t.Fatalf("trae upstream calls = %d, want 1", traeCalls)
	}
	if wbCalls != 0 {
		t.Fatalf("workbuddy upstream calls = %d, want 0（Trae 请求不得打到 WorkBuddy）", wbCalls)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %v, want Hello world", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

// 流式 Trae 请求要产出 OpenAI SSE chunk 并以 [DONE] 收尾。
func TestChatTraeStreamingProducesOpenAISSE(t *testing.T) {
	wbCalls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		wbCalls++
		return 200, sseOK, true
	})
	traeCalls := 0
	tc := traeTestClient(t, &traeCalls)

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	body := `{"model":"trae:cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"Hello world"`) {
		t.Fatalf("stream body missing content delta:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("stream body missing [DONE]:\n%s", out)
	}
	if wbCalls != 0 {
		t.Fatalf("workbuddy upstream calls = %d, want 0", wbCalls)
	}
}

// WorkBuddy 请求（裸名过渡语义）仍然只走 WorkBuddy 客户端。
func TestChatBareModelStillRoutesWorkBuddy(t *testing.T) {
	wbCalls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		wbCalls++
		return 200, sseOK, true
	})
	traeCalls := 0
	tc := traeTestClient(t, &traeCalls)

	wbAuth := &auth.Auth{UID: "wb1", AccessToken: "wb-at", ExpiresAt: 9999999999}
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(wbAuth, traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if wbCalls != 1 || traeCalls != 0 {
		t.Fatalf("wbCalls=%d traeCalls=%d, want 1/0", wbCalls, traeCalls)
	}
}

// /v1/models 要带 provider 前缀列出 Trae 模型（含 channel：Official 走 work）。
func TestModelsIncludesTraeEntries(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.AgentHost = "https://trae.fake"
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != upstream.TraeEpModels {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("nope")), Header: http.Header{}}, nil
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"config_info_list":[` +
				`{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}},` +
				`{"config_name":"DeepSeek-V4-Flash-Official","display_config":{"display_name":"Flash"}}]}`)),
		}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, m := range resp.Data {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	solo, ok := byID["trae:glm-5.2"]
	if !ok {
		t.Fatalf("trae:glm-5.2 missing; ids=%v", keysOf(byID))
	}
	if solo["owned_by"] != "trae" || solo["channel"] != "solo" {
		t.Fatalf("trae:glm-5.2 = %v", solo)
	}
	work, ok := byID["trae:DeepSeek-V4-Flash-Official"]
	if !ok {
		t.Fatalf("trae work model missing; ids=%v", keysOf(byID))
	}
	if work["channel"] != "work" {
		t.Fatalf("work model channel = %v, want work", work["channel"])
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
