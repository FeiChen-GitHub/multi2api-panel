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
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[]}`)))
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

// provider 未接入（Dispatcher 里 trae=nil）时必须 503，绝不回落 WorkBuddy 上游。
func TestChatTraeWithoutTraeClientReturns503(t *testing.T) {
	wbCalls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		wbCalls++
		return 200, sseOK, true
	})
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{
		Pool:       p,
		Upstream:   up,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), nil),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if wbCalls != 0 {
		t.Fatalf("workbuddy upstream calls = %d, want 0（Trae 未接入时不得误路由）", wbCalls)
	}
}

// work_enabled=false：Work 专属模型直接 503（不静默改走 SOLO），SOLO 模型不受影响。
func TestChatTraeWorkDisabled(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	var soloHits, workHits int
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case upstream.TraeEpCreateAgentTask:
			workHits++
		case upstream.TraeEpChat:
			soloHits++
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(traeSOLOFixture))}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{
		Pool:             p,
		Upstream:         up,
		Dispatcher:       upstream.NewDispatcher(upstream.NewWBClient(up), tc),
		TraeWorkDisabled: true,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("work model with work disabled: code=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
	if workHits != 0 {
		t.Fatalf("work upstream calls = %d, want 0", workHits)
	}

	// SOLO 模型不受 work_enabled 影响。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("solo model with work disabled: code=%d body=%s, want 200", rec2.Code, rec2.Body.String())
	}
	if soloHits != 1 {
		t.Fatalf("solo upstream calls = %d, want 1", soloHits)
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Work 专属模型必须走 Work 通道（api5-normal + create_agent_task）。
func TestChatTraeWorkModelRoutesToWorkChannel(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	var soloHits, workHits int
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case upstream.TraeEpCreateAgentTask:
			workHits++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(traeSOLOFixture))}, nil
		case upstream.TraeEpChat:
			soloHits++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(traeSOLOFixture))}, nil
		default:
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("nope"))}, nil
		}
	})}
	tc.StreamHTTP = tc.HTTP

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if workHits != 1 || soloHits != 0 {
		t.Fatalf("workHits=%d soloHits=%d, want 1/0", workHits, soloHits)
	}
}

// SOLO 撞权益不足（1005）时，同一请求内降级到 Work 通道重试一次。
func TestChatTraeFallsBackToWorkOnPlanLimit(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	var soloHits, workHits int
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case upstream.TraeEpChat:
			soloHits++
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":1005,"message":"plan limit"}`)),
			}, nil
		case upstream.TraeEpCreateAgentTask:
			workHits++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(traeSOLOFixture))}, nil
		default:
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("nope"))}, nil
		}
	})}
	tc.StreamHTTP = tc.HTTP

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (fallback should succeed on Work channel)", rec.Code, rec.Body.String())
	}
	if soloHits != 1 || workHits != 1 {
		t.Fatalf("soloHits=%d workHits=%d, want 1/1", soloHits, workHits)
	}
}
