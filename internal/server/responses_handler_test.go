package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func responsesTestHandler(t *testing.T, sse string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sse, true })
	traeCalls := 0
	tc := traeTestClient(t, &traeCalls)
	a, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	return NewHandler(Config{
		Pool:       testPoolWith(a),
		Upstream:   up,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc),
	})
}

// 非流式端到端：Responses 请求 → 上游 → response 对象。
func TestResponsesEndpointNonStreaming(t *testing.T) {
	h := responsesTestHandler(t, sseOK)
	body := `{"model":"trae:cn:glm-5.2","input":"hi","instructions":"be brief"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if resp["object"] != "response" {
		t.Fatalf("object = %v, want response", resp["object"])
	}
	if outs, _ := resp["output"].([]any); len(outs) == 0 {
		t.Fatalf("output 为空: %#v", resp)
	}
}

// 流式端到端：事件在流结束前就到了（真 server + 真 client；
// httptest.ResponseRecorder 的 Flush 是空实现，无法验证 Review Focus #1）。
func TestResponsesEndpointStreaming(t *testing.T) {
	h := responsesTestHandler(t, sseOK)
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/responses",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","input":"hi","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	buf := make([]byte, 8192)
	var seen string
	for !strings.Contains(seen, "event: response.completed") {
		n, rerr := br.Read(buf)
		seen += string(buf[:n])
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(seen, "event: response.created") {
		t.Fatalf("未见 response.created:\n%s", seen)
	}
	if !strings.Contains(seen, "event: response.completed") {
		t.Fatalf("未见 response.completed:\n%s", seen)
	}
}

// chat 路由零回归：同一条上游流在 chat 侧仍是原样 chat chunk。
func TestChatCompletionsUnchangedAfterResponses(t *testing.T) {
	h := responsesTestHandler(t, sseOK)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "event: response.") {
		t.Fatalf("chat 路由被污染成 Responses 事件:\n%s", body)
	}
	if !strings.Contains(body, "data: ") {
		t.Fatalf("chat 路由应仍是 chat chunk SSE:\n%s", body)
	}
}

// previous_response_id → 400（未开流时的错误体形状）。
func TestResponsesEndpointRejectsPreviousResponseID(t *testing.T) {
	h := responsesTestHandler(t, sseOK)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","input":"hi","previous_response_id":"resp_x"}`)))
	if rr.Code != 400 {
		t.Fatalf("code=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	e, _ := body["error"].(map[string]any)
	if e["code"] != "unsupported_parameter" {
		t.Fatalf("error.code = %v, want unsupported_parameter", e["code"])
	}
}
