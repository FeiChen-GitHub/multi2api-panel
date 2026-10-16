package server

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestResponsesRequestToChatStringInput(t *testing.T) {
	raw := []byte(`{"model":"trae:cn:glm-5.2","input":"你好","stream":true}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if got["model"] != "trae:cn:glm-5.2" || got["stream"] != true {
		t.Fatalf("model/stream = %v/%v", got["model"], got["stream"])
	}
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v", got["messages"])
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "你好" {
		t.Fatalf("msg = %#v", m)
	}
}

// harness 重建上下文后的主路径：input 混着上一轮的 function_call 与
// function_call_output。call_id 必须严格往返（Review Focus #3）。
func TestResponsesRequestToChatFunctionCallRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"trae:cn:glm-5.2","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"跑一下"}]},
		{"type":"function_call","call_id":"call_abc","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"call_abc","output":"a.txt"}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3: %s", len(msgs), out)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("msgs[1].role = %v, want assistant", asst["role"])
	}
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %#v", tcs)
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_abc" {
		t.Fatalf("tool_calls[0].id = %v, want call_abc", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "shell" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("function = %#v", fn)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_abc" || tool["content"] != "a.txt" {
		t.Fatalf("tool msg = %#v", tool)
	}
}

func TestResponsesRequestToChatRejectsPreviousResponseID(t *testing.T) {
	_, err := responsesRequestToChat([]byte(`{"model":"trae:cn:glm-5.2","input":"hi","previous_response_id":"resp_1"}`))
	var re *responsesError
	if !errors.As(err, &re) || re.Code != "unsupported_parameter" || re.Status != 400 {
		t.Fatalf("err = %v, want responsesError{400, unsupported_parameter}", err)
	}
}

func TestResponsesRequestToChatRejectsUnknownItemType(t *testing.T) {
	_, err := responsesRequestToChat([]byte(`{"model":"trae:cn:glm-5.2","input":[{"type":"mystery","x":1}]}`))
	var re *responsesError
	if !errors.As(err, &re) || re.Code != "unsupported_input_item" {
		t.Fatalf("err = %v, want responsesError{unsupported_input_item}", err)
	}
}

// type 省略/为空的消息项必须按消息处理：EasyInputMessage 的 type 是可选字段，
// 若干客户端（含 Codex 的部分版本）发 {"role","content"} 时不带 type。
func TestResponsesRequestToChatAcceptsMessageWithoutType(t *testing.T) {
	raw := []byte(`{"model":"trae:cn:glm-5.2","input":[
		{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"role":"assistant","content":"ok"}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %#v, want 2", got["messages"])
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "hi" {
		t.Fatalf("msgs[0] = %#v", m)
	}
	if m := msgs[1].(map[string]any); m["role"] != "assistant" || m["content"] != "ok" {
		t.Fatalf("msgs[1] = %#v", m)
	}
}

func TestResponsesRequestToChatInstructionsToolsAndTokens(t *testing.T) {
	raw := []byte(`{"model":"wb:cn:auto","input":"hi","instructions":"be brief",
		"tools":[{"type":"function","name":"shell","description":"d","parameters":{"type":"object"}}],
		"tool_choice":"auto","reasoning":{"effort":"high"},"max_output_tokens":64,
		"temperature":0.3,"top_p":0.9,"prompt_cache_key":"ck1","metadata":{"conversation_id":"cv1"},
		"store":true,"include":["reasoning.encrypted_content"],"parallel_tool_calls":true}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if got["reasoning_effort"] != "high" || got["max_tokens"] != float64(64) {
		t.Fatalf("effort/max_tokens = %v/%v", got["reasoning_effort"], got["max_tokens"])
	}
	if got["prompt_cache_key"] != "ck1" {
		t.Fatalf("prompt_cache_key 未透传: %v", got["prompt_cache_key"])
	}
	md, _ := got["metadata"].(map[string]any)
	if md["conversation_id"] != "cv1" {
		t.Fatalf("metadata = %#v", got["metadata"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("instructions 未置于首位: %s", out)
	}
	tools := got["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" || fn["description"] != "d" {
		t.Fatalf("tool 未转嵌套: %#v", tools[0])
	}
}

func TestResponsesRequestToChatSkipsReasoningItemsAndNoToolsKey(t *testing.T) {
	raw := []byte(`{"model":"trae:cn:glm-5.2","input":[
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"xx"},
		{"type":"message","role":"user","content":"yo"}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if _, ok := got["tools"]; ok {
		t.Fatalf("tools 缺失时不应写该键: %s", out)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("reasoning item 应被丢弃: %s", out)
	}
}

func TestResponsesRequestToChatRejectsMissingInput(t *testing.T) {
	_, err := responsesRequestToChat([]byte(`{"model":"trae:cn:glm-5.2"}`))
	var re *responsesError
	if !errors.As(err, &re) || re.Code != "invalid_request" || re.Status != 400 {
		t.Fatalf("err = %v, want responsesError{400, invalid_request}", err)
	}
}

// Codex CLI 用 {"type":"namespace"} 容器装 MCP 工具组，并常与 {"type":"tool_search"}
// 这类托管工具混在同一个 tools 数组里发。此前只要有一个非 function 就整请求 422
// （实测客户端报 unsupported tool type "namespace"，链路完全不可用）。
// 现在：namespace 内部 function 保持原名提升到顶层；不认识的托管工具跳过。
func TestResponsesRequestToChatNamespaceTools(t *testing.T) {
	raw := []byte(`{"model":"trae:cn:glm-5.2","input":"hi","tools":[
		{"type":"function","name":"top_fn","description":"顶层工具","parameters":{"type":"object"}},
		{"type":"namespace","name":"mcp__server","description":"MCP 工具组","tools":[
			{"type":"function","name":"inner_fn","parameters":{"type":"object"}},
			{"type":"custom","name":"grammar_tool"}]},
		{"type":"tool_search"}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v（namespace / tool_search 不应再 422）", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	list, ok := got["tools"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("tools = %v, want 2 条（顶层 1 + namespace 内 1，custom 跳过）", got["tools"])
	}
	names := make([]string, 0, len(list))
	for _, raw := range list {
		fn := raw.(map[string]any)["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	if names[0] != "top_fn" || names[1] != "inner_fn" {
		t.Fatalf("tool names = %v, want [top_fn inner_fn]（namespace 名不参与拼接）", names)
	}
}

// tools 里只有托管工具时：全部跳过 → 不写 tools 键（与"无工具"同义，
// ToolNameAllowlist 据此不启用 DSML 修复）。
func TestResponsesRequestToChatOnlyHostedTools(t *testing.T) {
	out, err := responsesRequestToChat([]byte(`{"model":"trae:cn:glm-5.2","input":"hi","tools":[{"type":"web_search"}]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if _, ok := got["tools"]; ok {
		t.Fatalf("只剩托管工具时不应写 tools 键: %s", out)
	}
}
