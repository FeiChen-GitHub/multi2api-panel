package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 一段实测形态的 SOLO 流：metadata / output（含思考链与工具调用分片）/ token_usage / done。
const traeSSOFixture = `id:1
event:metadata
data:{"model":"glm-5.2","session_id":"s1"}

event:output
data:{"response":"Hello","reasoning_content":"think"}

event:output
data:{"response":" world","tool_calls":[{"index":0,"id":"call_1","type":"function","function_call":{"name":"f","arguments":"{\"a\":"}}]}

event:output
data:{"tool_calls":[{"index":0,"function_call":{"arguments":"1}"}}]}

event:token_usage
data:{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}

event:done
data:{"finish_reason":"stop"}

`

// 非流式聚合要把 SOLO 流拼成一个 OpenAI chat.completion（含工具调用分片合并）。
func TestTraeAggregateBuildsOpenAICompletion(t *testing.T) {
	resp, err := TraeAggregate(strings.NewReader(traeSSOFixture), "glm-5.2")
	if err != nil {
		t.Fatalf("TraeAggregate: %v", err)
	}
	if resp["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", resp["object"])
	}
	if resp["model"] != "glm-5.2" {
		t.Fatalf("model = %v, want glm-5.2", resp["model"])
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %q, want %q", msg["content"], "Hello world")
	}
	if msg["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %v, want think", msg["reasoning_content"])
	}
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %#v, want 1 条", msg["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" {
		t.Fatalf("tool name = %v, want f", fn["name"])
	}
	if fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool arguments = %v, want merged {\"a\":1}", fn["arguments"])
	}
	usage, ok := resp["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(8) {
		t.Fatalf("usage = %#v, want total_tokens=8", resp["usage"])
	}
}

// 流内业务错误（1005 权益不足）必须能被分类成 hard credit，供上层冷却账号。
func TestTraeAggregateStreamErrorKind(t *testing.T) {
	fixture := "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n"
	_, err := TraeAggregate(strings.NewReader(fixture), "glm-5.2")
	if err == nil {
		t.Fatal("want error for event:error stream")
	}
	se, ok := err.(*TraeStreamError)
	if !ok {
		t.Fatalf("err type = %T, want *TraeStreamError", err)
	}
	if se.Kind() != ErrHardCredit {
		t.Fatalf("Kind() = %v, want ErrHardCredit (1005 权益不足 → 长冷却)", se.Kind())
	}
}

// 流式转换要产出 OpenAI chunk 并以 [DONE] 收尾；错误事件走 onErr 回调。
func TestTraeStreamToOpenAIChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	sawErr := false
	err := TraeStreamToOpenAI(rec, strings.NewReader(traeSSOFixture), "glm-5.2", func(*TraeStreamError) { sawErr = true })
	if err != nil {
		t.Fatalf("TraeStreamToOpenAI: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) {
		t.Fatalf("stream body missing first content delta:\n%s", body)
	}
	if !strings.Contains(body, `"content":" world"`) {
		t.Fatalf("stream body missing second content delta:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("stream body missing finish_reason:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body missing [DONE]:\n%s", body)
	}
	if sawErr {
		t.Fatal("onErr should not fire for a clean stream")
	}
}

// 流式遇到 event:error 要回调，并仍然以 [DONE] 收尾（客户端不会挂住）。
func TestTraeStreamToOpenAIReportsErrorAndTerminates(t *testing.T) {
	rec := httptest.NewRecorder()
	var got *TraeStreamError
	fixture := "event:error\ndata:{\"code\":4011,\"message\":\"too many\"}\n\n"
	if err := TraeStreamToOpenAI(rec, strings.NewReader(fixture), "glm-5.2", func(se *TraeStreamError) { got = se }); err != nil {
		t.Fatalf("TraeStreamToOpenAI: %v", err)
	}
	if got == nil {
		t.Fatal("onErr was not called for event:error")
	}
	if got.Kind() != ErrSoftRate {
		t.Fatalf("Kind() = %v, want ErrSoftRate (4011)", got.Kind())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream must still terminate with [DONE]:\n%s", rec.Body.String())
	}
}
