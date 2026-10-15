package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// chatCompletionToResponse 把内部 chat.completion 对象转成 Responses 的
// response 对象（spec §10）。恒为 store=false 的无状态响应。
func chatCompletionToResponse(chat map[string]any) map[string]any {
	id := str(chat["id"])
	if id == "" {
		id = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	out := map[string]any{
		"id":         "resp_" + id,
		"object":     "response",
		"status":     "completed",
		"store":      false,
		"created_at": time.Now().Unix(),
		"model":      chat["model"],
	}
	if m, ok := out["model"]; !ok || m == nil {
		out["model"] = ""
	}

	var output []any
	if msg, ok := chat["choices"].([]any); ok && len(msg) > 0 {
		choice, _ := msg[0].(map[string]any)
		m, _ := choice["message"].(map[string]any)
		content := str(m["content"])
		if content != "" {
			output = append(output, map[string]any{
				"type":   "message",
				"role":   "assistant",
				"id":     "msg_" + str(chat["id"]),
				"status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": content, "annotations": []any{},
				}},
			})
		}
		for _, raw := range sliceOf(m["tool_calls"]) {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			callID := str(tc["id"])
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        "fc_" + callID,
				"call_id":   callID,
				"name":      str(fn["name"]),
				"arguments": str(fn["arguments"]),
				"status":    "completed",
			})
		}
	}
	if output == nil {
		output = []any{}
	}
	out["output"] = output

	if usage, ok := chat["usage"].(map[string]any); ok {
		u := map[string]any{}
		rename(usage, u, "prompt_tokens", "input_tokens")
		rename(usage, u, "completion_tokens", "output_tokens")
		rename(usage, u, "total_tokens", "total_tokens")
		if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
			u["output_tokens_details"] = d
		}
		if p, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			u["input_tokens_details"] = p
		}
		out["usage"] = u
	}
	return out
}

func rename(src, dst map[string]any, from, to string) {
	if v, ok := src[from]; ok {
		dst[to] = v
	}
}

func sliceOf(v any) []any {
	s, _ := v.([]any)
	return s
}

// writeResponsesError 写 Responses 形状的错误体（HTTP 头尚未发出时）。
func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    responsesErrorType(code),
			"code":    code,
			"message": msg,
		},
	})
}

func responsesErrorType(code string) string {
	switch code {
	case "invalid_request", "unsupported_parameter", "unsupported_input_item",
		"invalid_request_error", "missing_required_parameter":
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// hint 非空时附 error.gateway_hint（与 writeOpenAIErrorHint 语义一致）。
func writeResponsesErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeResponsesError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":         responsesErrorType(code),
			"code":         code,
			"message":      msg,
			"gateway_hint": strings.TrimSpace(hint),
		},
	})
}
