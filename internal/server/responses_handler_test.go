package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
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

// responsesTestWBHandler 走 WorkBuddy 通道（假上游按 sse 返回内容）。
func responsesTestWBHandler(t *testing.T, sse string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sse, true })
	wb := &auth.Auth{UID: "wb1", AccessToken: "wb-at", ExpiresAt: 9999999999}
	return NewHandler(Config{Pool: testPoolWith(wb), Upstream: up})
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

// TestResponsesEndpointDoesNotResendHistoryImage 端到端回归（2026-11-05 实案）：
// 第二轮请求回放第一轮截图后，**出站给上游的 body** 不得再带那张图。
//
// 分层意义：上面的 responsesRequestToChat 单测只验证转换函数；这条覆盖
// responsesCompletions → chatCore → prompt.Rewrite → prepareBody 整条管线，
// 确认后续阶段不会把图又捡回来。
func TestResponsesEndpointDoesNotResendHistoryImage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

	var sentBodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBodies = append(sentBodies, raw)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	wb := &auth.Auth{UID: "wb1", AccessToken: "wb-at", ExpiresAt: 9999999999}
	h := NewHandler(Config{Pool: testPoolWith(wb), Upstream: up})

	// 第 1 轮：用户发话 → agent 截图。
	first := `{"model":"wb:cn:auto","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我看看这个界面"}]},
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + dataURL + `"}]}]}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(first)))
	if rr.Code != 200 {
		t.Fatalf("round1 code=%d body=%s", rr.Code, rr.Body.String())
	}

	// 第 2 轮：客户端回放完整历史 + 新提问。图属于上一轮 → 不得上提。
	second := `{"model":"wb:cn:auto","stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我看看这个界面"}]},
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + dataURL + `"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"这是你的桌面"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}]}`
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(second)))
	if rr.Code != 200 {
		t.Fatalf("round2 code=%d body=%s", rr.Code, rr.Body.String())
	}

	if len(sentBodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(sentBodies))
	}
	// 第 1 轮必须带图（本回合的截图，模型得看得见）。
	if !bytes.Contains(sentBodies[0], []byte(dataURL)) {
		t.Fatalf("round1 出站 body 丢了本回合截图（模型会答「看不到图」）: %s", sentBodies[0])
	}
	// 第 2 轮必须不带图（历史截图不得被重复上提）。
	if bytes.Contains(sentBodies[1], []byte(dataURL)) {
		t.Fatalf("round2 出站 body 仍带着上一轮的截图（每轮重复计费 + 语义错位）: %s", sentBodies[1])
	}
}

// Review Focus #2：转换后的 body 仍要能被 session.ExtractKey 认出粘性键——
// 否则 Responses 客户端的粘性静默退化成内容派生键（能跑，但 prompt cache
// 命中率下降）。
func TestResponsesConvertedBodyStillYieldsStickyKey(t *testing.T) {
	for _, raw := range []string{
		`{"model":"trae:cn:glm-5.2","input":"hi","prompt_cache_key":"ck1"}`,
		`{"model":"trae:cn:glm-5.2","input":"hi","metadata":{"conversation_id":"cv1"}}`,
	} {
		chatBody, err := responsesRequestToChat([]byte(raw))
		if err != nil {
			t.Fatalf("convert %s: %v", raw, err)
		}
		if session.ExtractKey(chatBody) == "" {
			t.Fatalf("转换后 body 丢了粘性键: %s -> %s", raw, chatBody)
		}
	}
}

// Review Focus #3：call_id 必须跨「请求回填 → 响应产出」严格往返，
// 否则客户端把工具结果当孤儿（工具跑完了但模型接不上）。
func TestResponsesCallIDRoundTripThroughHandler(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_xyz\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	h := responsesTestWBHandler(t, sse)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"wb:cn:auto","input":[{"type":"function_call","call_id":"call_xyz","name":"shell","arguments":"{}"}],"stream":true}`)))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("[DONE] 必须吞掉:\n%s", body)
	}
	if !strings.Contains(body, `"call_id":"call_xyz"`) {
		t.Fatalf("call_id 未往返:\n%s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("工具调用流未收尾:\n%s", body)
	}
}
