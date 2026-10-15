package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestChatCompletionToResponseText(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-abc",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "答案"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(11), "completion_tokens": float64(5), "total_tokens": float64(16),
		},
	}
	out := chatCompletionToResponse(chat)
	if out["object"] != "response" || out["status"] != "completed" || out["store"] != false {
		t.Fatalf("object/status/store = %v/%v/%v", out["object"], out["status"], out["store"])
	}
	id, _ := out["id"].(string)
	if len(id) < 5 || id[:5] != "resp_" {
		t.Fatalf("id = %q, want resp_ 前缀", id)
	}
	outs := out["output"].([]any)
	if len(outs) != 1 {
		t.Fatalf("output len = %d, want 1", len(outs))
	}
	item := outs[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("item = %#v", item)
	}
	part := item["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "答案" {
		t.Fatalf("content part = %#v", part)
	}
	u := out["usage"].(map[string]any)
	if u["input_tokens"] != float64(11) || u["output_tokens"] != float64(5) || u["total_tokens"] != float64(16) {
		t.Fatalf("usage = %#v", u)
	}
}

// 工具调用存在时不额外产空 message item（否则 Codex 会收到一个空回答）。
func TestChatCompletionToResponseToolCall(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-abc",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	out := chatCompletionToResponse(chat)
	outs := out["output"].([]any)
	if len(outs) != 1 {
		t.Fatalf("output len = %d, want 1: %#v", len(outs), outs)
	}
	item := outs[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_1" || item["name"] != "shell" {
		t.Fatalf("item = %#v", item)
	}
	if item["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %v", item["arguments"])
	}
}

func TestWriteResponsesErrorShape(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesError(rr, 400, "unsupported_parameter", "nope")
	if rr.Code != 400 {
		t.Fatalf("code = %d", rr.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	e := body["error"].(map[string]any)
	for _, k := range []string{"type", "code", "message"} {
		if _, ok := e[k]; !ok {
			t.Fatalf("error 缺字段 %q: %#v", k, e)
		}
	}
}

// 无 id 的上游响应也必须拿到合法 resp_ id。
func TestChatCompletionToResponseNoID(t *testing.T) {
	out := chatCompletionToResponse(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "x"}}},
	})
	id, _ := out["id"].(string)
	if len(id) < 5 || id[:5] != "resp_" {
		t.Fatalf("id = %q, want resp_ 前缀", id)
	}
}
