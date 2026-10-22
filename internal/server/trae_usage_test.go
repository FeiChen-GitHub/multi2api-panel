package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
	"io"
)

// SOLO 流 + token_usage：统计侧必须把 prompt/completion/total 记进用量。
const traeUsageFixture = "event:output\ndata:{\"response\":\"hi\"}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// traeUsageTestClient 返回一个把 SOLO 对话端点固定为 fixture 的 TraeClient。
func traeUsageTestClient(t *testing.T, fixture string) *upstream.TraeClient {
	t.Helper()
	tc := upstream.NewTraeClient()
	tc.AgentHost = "https://trae.fake"
	tc.UgHost = "https://trae.fake"
	tc.OAuthHost = "https://trae.fake"
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != upstream.TraeEpChat {
			t.Logf("unexpected trae request path: %s", r.URL.Path)
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(fixture)),
		}, nil
	})}
	tc.HTTP = fake
	tc.StreamHTTP = fake
	return tc
}

func newTraeUsageHandler(t *testing.T, fixture string) (*pool.Pool, *Handler) {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("unexpected workbuddy upstream call")
		return 500, "", false
	})
	tc := traeUsageTestClient(t, fixture)
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc), Usage: usage.New("")})
	return p, h
}

func traeUsagePost(t *testing.T, h *Handler, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]`
	if stream {
		body += `,"stream":true`
	}
	body += `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func checkTraeUsage(t *testing.T, p *pool.Pool, h *Handler) {
	t.Helper()
	st, ok := p.Status("trae1")
	if !ok {
		t.Fatalf("pool status missing for trae1")
	}
	if st.TokenUsage.RequestCount != 1 {
		t.Errorf("TokenUsage.RequestCount = %d, want 1", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 1 {
		t.Errorf("TokenUsage.UsageCount = %d, want 1", st.TokenUsage.UsageCount)
	}
	if st.TokenUsage.PromptTokens != 3 || st.TokenUsage.CompletionTokens != 5 || st.TokenUsage.TotalTokens != 8 {
		t.Errorf("TokenUsage tokens = (%d,%d,%d), want (3,5,8)",
			st.TokenUsage.PromptTokens, st.TokenUsage.CompletionTokens, st.TokenUsage.TotalTokens)
	}
	rec2 := h.cfg.Usage.Snapshot(24, nil)
	if rec2.Totals.Requests != 1 {
		t.Errorf("Usage Snapshot Totals.Requests = %d, want 1", rec2.Totals.Requests)
	}
	if rec2.Totals.PromptTokens != 3 || rec2.Totals.CompletionTok != 5 || rec2.Totals.TotalTokens != 8 {
		t.Errorf("Usage Snapshot Totals tokens = (%d,%d,%d), want (3,5,8)",
			rec2.Totals.PromptTokens, rec2.Totals.CompletionTok, rec2.Totals.TotalTokens)
	}
}

// SOLO 非流式：聚合响应带 usage → 池统计与用量界面都要记上。
func TestTraeUsageNonStreamSolo(t *testing.T) {
	p, h := newTraeUsageHandler(t, traeUsageFixture)
	rec := traeUsagePost(t, h, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	checkTraeUsage(t, p, h)
}

// SOLO 流式：token_usage 帧 → 统计同样要记上。
func TestTraeUsageStreamSolo(t *testing.T) {
	p, h := newTraeUsageHandler(t, traeUsageFixture)
	rec := traeUsagePost(t, h, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream missing [DONE]:\n%s", rec.Body.String())
	}
	checkTraeUsage(t, p, h)
}

// 成功但没有 token_usage：仍要计 Requests（usage_count 为 0）。
func TestTraeUsageSuccessWithoutUsage(t *testing.T) {
	noUsage := "event:output\ndata:{\"response\":\"hi\"}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	p, h := newTraeUsageHandler(t, noUsage)
	rec := traeUsagePost(t, h, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("trae1")
	if !ok {
		t.Fatalf("pool status missing for trae1")
	}
	if st.TokenUsage.RequestCount != 1 {
		t.Errorf("TokenUsage.RequestCount = %d, want 1", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 0 {
		t.Errorf("TokenUsage.UsageCount = %d, want 0", st.TokenUsage.UsageCount)
	}
}

// 流内 error 帧：不计入请求数。
func TestTraeUsageStreamErrorNotCounted(t *testing.T) {
	errFixture := "event:error\ndata:{\"code\":6004,\"message\":\"x\"}\n\n"
	p, h := newTraeUsageHandler(t, errFixture)
	rec := traeUsagePost(t, h, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("trae1")
	if !ok {
		t.Fatalf("pool status missing for trae1")
	}
	if st.TokenUsage.RequestCount != 0 {
		t.Errorf("TokenUsage.RequestCount = %d, want 0", st.TokenUsage.RequestCount)
	}
}
