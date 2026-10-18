package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
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

// Trae HTTP 非 2xx 的失败摘要必须落到请求归档：业务码/原文是区分限流、
// 权益不足和参数错误的关键，不能只剩一个 503。
func TestTraeHTTPErrorCapturesFailureSummary(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 429,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":4011,"message":"too many requests"}`)),
		}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: reqLog,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc), TraeWorkDisabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s, want 429", rec.Code, rec.Body)
	}
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent = %+v", s.Recent)
	}
	e := s.Recent[0]
	if e.ErrorCode != "soft_rate" || !strings.Contains(e.ErrorMessage, "4011") {
		t.Fatalf("failure summary = code %q msg %q", e.ErrorCode, e.ErrorMessage)
	}
}

// Trae 流内业务错误也要带具体业务码（如 4011 限流），而不是统一 503。
func TestTraeStreamErrorCapturesFailureSummary(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:error\ndata:{\"code\":4011,\"message\":\"too many\"}\n\n")),
		}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: reqLog,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc), TraeWorkDisabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent = %+v", s.Recent)
	}
	e := s.Recent[0]
	if e.ErrorCode != "trae_stream_4011" || !strings.Contains(e.ErrorMessage, "too many") {
		t.Fatalf("failure summary = code %q msg %q", e.ErrorCode, e.ErrorMessage)
	}
}

const traeUsageSSE = "event:output\ndata:{\"response\":\"ok\"}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func traeClientReturning(body string) *upstream.TraeClient {
	tc := upstream.NewTraeClient()
	tc.AgentHost = "https://trae.fake"
	tc.UgHost = "https://trae.fake"
	tc.OAuthHost = "https://trae.fake"
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	tc.HTTP = fake
	tc.StreamHTTP = fake
	return tc
}

// Trae 的 token_usage 必须进入账号累计器与面板用量账本。上游实测没有
// credit 字段，因此只能记 Token 历史，不能伪造积分扣除行。
func TestTraeUsageRecordedWithoutCredit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
			tc := traeClientReturning(traeUsageSSE)
			a, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
			if err != nil {
				t.Fatalf("parse trae auth: %v", err)
			}
			p := testPoolWith(a)
			rec := usage.New("")
			h := NewHandler(Config{
				Pool:             p,
				Upstream:         up,
				Dispatcher:       upstream.NewDispatcher(upstream.NewWBClient(up), tc),
				Usage:            rec,
				TraeWorkDisabled: true,
			})
			body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"trae:cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if rr.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
			}

			st, ok := p.Status("trae1")
			if !ok {
				t.Fatal("trae account missing from pool")
			}
			if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
				st.TokenUsage.PromptTokens != 11 || st.TokenUsage.CompletionTokens != 7 ||
				st.TokenUsage.TotalTokens != 18 || st.TokenUsage.LastModel != "glm-5.2" {
				t.Fatalf("pool token usage = %+v", st.TokenUsage)
			}

			snap := rec.Snapshot(24, nil)
			if snap.Totals.Requests != 1 || snap.Totals.PromptTokens != 11 ||
				snap.Totals.CompletionTok != 7 || snap.Totals.TotalTokens != 18 {
				t.Fatalf("usage totals = %+v", snap.Totals)
			}
			if snap.Totals.CreditSamples != 0 || snap.Totals.Credits != 0 ||
				len(snap.CreditByAccount) != 0 || len(snap.CreditByModel) != 0 {
				t.Fatalf("Trae 无 credit 时不得生成积分扣除行: totals=%+v account=%+v model=%+v",
					snap.Totals, snap.CreditByAccount, snap.CreditByModel)
			}
			if len(snap.ByAccount) != 1 || snap.ByAccount[0].Key != "trae1" ||
				len(snap.ByModel) != 1 || snap.ByModel[0].Key != "glm-5.2" {
				t.Fatalf("usage dimensions = account %+v model %+v", snap.ByAccount, snap.ByModel)
			}
		})
	}
}

// 若未来 Trae 上游确实回传单次 credit，必须按真实值累计；不能因为当前
// Trae 默认没有该字段就永久丢弃它。
func TestTraeUsageHonorsExplicitCredit(t *testing.T) {
	const body = "event:output\ndata:{\"response\":\"ok\"}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"credit\":0.75}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := traeClientReturning(body)
	a, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(a)
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:             p,
		Upstream:         up,
		Dispatcher:       upstream.NewDispatcher(upstream.NewWBClient(up), tc),
		Usage:            rec,
		TraeWorkDisabled: true,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	snap := rec.Snapshot(24, nil)
	if snap.Totals.Credits != 0.75 || snap.Totals.CreditSamples != 1 || snap.Totals.CreditTokens != 15 {
		t.Fatalf("explicit credit totals = %+v", snap.Totals)
	}
	if len(snap.CreditByAccount) != 1 || snap.CreditByAccount[0].Key != "trae1" {
		t.Fatalf("credit_by_account = %+v", snap.CreditByAccount)
	}
}

// Work 通道的 token_usage 藏在 payload 里，也必须走同一用量账本。
func TestTraeWorkUsageRecordedWithoutCredit(t *testing.T) {
	const workSSE = "data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"ok\"}]}}\n\n" +
		"data: {\"event\":\"token_usage\",\"payload\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n" +
		"data: {\"event\":\"done\",\"payload\":{}}\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := workSSE
		if r.URL.Path == upstream.TraeEpChat {
			// Work 成功后异步探测 work_credits；给一个合法快照，避免测试日志噪音。
			body = "data: {\"cn_credits_remain_info\":{\"ide_credits\":1,\"work_credits\":2}}\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	tc.StreamHTTP = tc.HTTP
	a, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(a)
	rec := usage.New("")
	h := NewHandler(Config{
		Pool:       p,
		Upstream:   up,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc),
		Usage:      rec,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:DeepSeek-V4-Flash-Official","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	st, _ := p.Status("trae1")
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 || st.TokenUsage.TotalTokens != 8 {
		t.Fatalf("pool token usage = %+v", st.TokenUsage)
	}
	snap := rec.Snapshot(24, nil)
	if snap.Totals.TotalTokens != 8 || snap.Totals.CreditSamples != 0 {
		t.Fatalf("usage totals = %+v", snap.Totals)
	}
}
