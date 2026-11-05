package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
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

// currentTurnStart 的边界：type 省略/空、role 省略、非 message 类型都要按
// responsesMessageItem 的口径判定，否则回合边界算错 → 图被错误上提或漏提。
func TestCurrentTurnStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  int
	}{
		{"无 user 项 → 0（全 input 视为本回合）", `[]`, 0},
		{"只有工具项 → 0", `[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]`, 0},
		{"单轮：user 在首", `[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]`, 1},
		{"跨轮：取最后一条 user 之后", `[{"type":"message","role":"user","content":"a"},{"type":"message","role":"user","content":"b"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]`, 2},
		{"type 省略按消息处理", `[{"role":"user","content":"hi"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]`, 1},
		{"role 省略按 user 处理", `[{"type":"message","content":"hi"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]`, 1},
		{"assistant 消息不算回合起点", `[{"type":"message","role":"user","content":"a"},{"type":"message","role":"assistant","content":"b"}]`, 1},
	} {
		var items []any
		if err := json.Unmarshal([]byte(tc.input), &items); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		if got := currentTurnStart(items); got != tc.want {
			t.Errorf("%s: currentTurnStart = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// function_call_output 携带图片 part 时：文本留 tool、图片挂到本回合的末条 user 消息。
//
// 2026-10-18 实案（11115 prompt is too long: 2059849 > 1048576）：
// Codex 的 view_image 把 2.4MB base64 作为 input_image 回传，旧实现 json.Marshal
// 成字符串 → 上游按文本分词 ≈ 2.05M token，一张截图顶爆 1M 上下文。
// 且实测上游只编码「最后一条消息」里的图，tool/历史消息里的图静默丢弃。
//
// 本用例是**单轮**形态：用户先发话，随后 agent 截图。图位于最后一条 user 消息之后
// → 属于本回合，必须上提。（跨轮形态见 TestResponsesRequestToChatDoesNotHoistHistoryToolImage。）
func TestResponsesRequestToChatFunctionCallOutputImagePart(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"看图"}]},
		{"type":"function_call","call_id":"call_img","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_img","output":[{"type":"input_image","image_url":"` + dataURL + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// 硬性回归守卫：base64 绝不能以序列化文本进 prompt（旧实现的形态）。
	if bytes.Contains(out, []byte(`\"input_image\"`)) {
		t.Fatalf("图片 part 被序列化成文本塞进 content: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4（user/assistant+tool + 合成的图片 user）: %s", len(msgs), out)
	}
	if msgs[2].(map[string]any)["role"] != "tool" {
		t.Fatalf("msgs[2].role = %v, want tool", msgs[2].(map[string]any)["role"])
	}
	if msgs[2].(map[string]any)["content"] != "" {
		t.Fatalf("tool content = %#v, want 空串（图片不进 tool 文本）", msgs[2].(map[string]any)["content"])
	}
	// 图片必须挂在最后一条 user 消息上（上游只编码末条消息的图）。
	lastMsg := msgs[len(msgs)-1].(map[string]any)
	if lastMsg["role"] != "user" {
		t.Fatalf("末条 role = %v, want user", lastMsg["role"])
	}
	parts, ok := lastMsg["content"].([]any)
	if !ok {
		t.Fatalf("末条 content = %#v, want parts 数组", lastMsg["content"])
	}
	var found bool
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["type"] != "image_url" {
			continue
		}
		found = true
		img, _ := pm["image_url"].(map[string]any)
		if img["url"] != dataURL {
			t.Fatalf("image url 丢失或改写")
		}
	}
	if !found {
		t.Fatalf("图片 part 未出现在末条 user 消息里: %#v", lastMsg["content"])
	}
}

// function_call_output 的图片以裸 data URL 字符串回传时同样要挪到 user 消息。
func TestResponsesRequestToChatFunctionCallOutputImageString(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"` + dataURL + `"}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3（assistant/tool/图片 user）: %s", len(msgs), out)
	}
	toolMsg := msgs[1].(map[string]any)
	if toolMsg["content"] != "" {
		t.Fatalf("tool content = %#v, want 空串（图片不进 tool 文本）", toolMsg["content"])
	}
	lastMsg := msgs[2].(map[string]any)
	parts, ok := lastMsg["content"].([]any)
	if !ok {
		t.Fatalf("图片消息 content = %#v, want parts 数组", lastMsg["content"])
	}
	var found bool
	for _, p := range parts {
		if pm, _ := p.(map[string]any); pm["type"] == "image_url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("裸 data URL 未转成 image part: %#v", lastMsg["content"])
	}
}

// TestResponsesRequestToChatDoesNotHoistHistoryToolImage 跨轮回归：**历史轮次**的工具截图
// 不得被搬进本轮的 user 消息。
//
// Bug 形态（2026-11-05 Claude Desktop 实案）：第 1 轮截图后，客户端每轮回放完整 input，
// 旧实现把历史上所有 function_call_output 的图无条件上提到末条 user 消息 →
// 模型看到的是「用户每轮都在发同一张图」，语义错位且每轮重复计 image token。
//
// 修复口径：只有「最后一条 user 消息之后」的工具图片才算本回合（currentTurnStart）。
func TestResponsesRequestToChatDoesNotHoistHistoryToolImage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"帮我看看这个界面"}]},
		{"type":"function_call","call_id":"call_shot","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_shot","output":[{"type":"input_image","image_url":"` + dataURL + `"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"这是你的桌面"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if bytes.Contains(out, []byte("data:image/")) {
		t.Fatalf("历史轮次的截图不得出现在本轮请求里（否则每轮都会重复上提）: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("末条 role = %v, want user", last["role"])
	}
	if c, ok := last["content"].(string); !ok || c != "继续" {
		t.Fatalf("末条 content = %#v, want 原样的字符串 %q（不得被改写成 parts）", last["content"], "继续")
	}
	if len(msgs) != 5 {
		t.Fatalf("messages len = %d, want 5（不应额外合成 user 消息）: %s", len(msgs), out)
	}
}

// TestResponsesRequestToChatHoistsOnlyLatestTurnImage 多轮里只有最后一条 user 之后的图
// 被上提：上一轮截图丢弃，本轮截图保留。
func TestResponsesRequestToChatHoistsOnlyLatestTurnImage(t *testing.T) {
	const oldImg = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	const newImg = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"第一轮"}]},
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + oldImg + `"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"第二轮，再截一次"}]},
		{"type":"function_call","call_id":"c2","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":[{"type":"input_image","image_url":"` + newImg + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if bytes.Contains(out, []byte(oldImg)) {
		t.Fatalf("上一轮的截图不应被上提: %s", out)
	}
	if !bytes.Contains(out, []byte(newImg)) {
		t.Fatalf("本轮的截图必须被上提（否则模型看不到图）: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	parts, ok := last["content"].([]any)
	if !ok {
		t.Fatalf("末条 content = %#v, want parts 数组", last["content"])
	}
	n := 0
	for _, p := range parts {
		if pm, _ := p.(map[string]any); pm["type"] == "image_url" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("末条 user 图片数 = %d, want 1（只挂本轮那张）: %s", n, out)
	}
}

// TestResponsesRequestToChatHoistsImageWithoutUserMessage 纯工具续跑（input 里没有
// 新的 user 项）必须保持原有上提行为——否则图永远上提不了、模型答「看不到图」。
func TestResponsesRequestToChatHoistsImageWithoutUserMessage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + dataURL + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("末条 role = %v, want user（无 user 项时合成一条）", last["role"])
	}
	if !bytes.Contains(out, []byte(dataURL)) {
		t.Fatalf("无 user 消息项时图仍须上提: %s", out)
	}
}

// TestResponsesRequestToChatSkipsImageAlreadyInUserMessage 末条 user 已带同一张图时
// 不重复挂载（翻译层可能把图同时写进 tool 结果与 user 消息）。
func TestResponsesRequestToChatSkipsImageAlreadyInUserMessage(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"看图"}]},
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + dataURL + `"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"看到了"},{"type":"input_image","image_url":"` + dataURL + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	parts, _ := last["content"].([]any)
	n := 0
	for _, p := range parts {
		if pm, _ := p.(map[string]any); pm["type"] == "image_url" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("末条 user 图片数 = %d, want 1（已有同图不得重复挂）: %s", n, out)
	}
}

// 纯文本 function_call_output（字符串 / {"text":...} / 多段文本数组）仍走字符串，
// 保持既有 chat 路径与 DSML 修复的字符串口径（回归守卫）。
func TestResponsesRequestToChatFunctionCallOutputTextUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"字符串", `{"type":"function_call_output","call_id":"c1","output":"a.txt"}`, "a.txt"},
		{"text 对象", `{"type":"function_call_output","call_id":"c1","output":{"text":"b.txt"}}`, "b.txt"},
		{"文本数组", `{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"c1"},{"type":"input_text","text":"c2"}]}`, "c1\nc2"},
	} {
		raw := []byte(`{"model":"wb:glm-5.3-flash","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` + tc.input + `]}`)
		out, err := responsesRequestToChat(raw)
		if err != nil {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		toolMsg := got["messages"].([]any)[1].(map[string]any)
		if toolMsg["content"] != tc.want {
			t.Fatalf("%s: content = %#v, want %q", tc.name, toolMsg["content"], tc.want)
		}
	}
}

// 同一张图在历史里出现多次时只挂一次（上游每图按 ~1-2k token 计，重复纯浪费）。
func TestResponsesRequestToChatDedupesImages(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"截图看看"}]},
		{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"` + dataURL + `"}]},
		{"type":"function_call","call_id":"c2","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"c2","output":[{"type":"input_image","image_url":"` + dataURL + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := got["messages"].([]any)
	parts, _ := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	n := 0
	for _, p := range parts {
		if pm, _ := p.(map[string]any); pm["type"] == "image_url" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("末条 user 图片数 = %d, want 1（同 URL 去重）: %s", n, out)
	}
}
// input_file 附件必须内联成文本，不能静默丢弃（否则模型答「附件呢？」）。
func TestResponsesRequestToChatInputFileText(t *testing.T) {
	body := "订单号 A12345，金额 99 元。"
	dataURL := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(body))
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"看附件"},
			{"type":"input_file","filename":"order.txt","file_data":"` + dataURL + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Contains(out, []byte("A12345")) {
		t.Fatalf("附件正文未内联进请求: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "A12345") {
		t.Fatalf("content = %q, want 含附件正文", content)
	}
}

// 二进制附件（PDF/图片）不能内联，但必须留下可诊断的说明，而不是静默消失。
func TestResponsesRequestToChatInputFileBinary(t *testing.T) {
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[
			{"type":"input_file","filename":"doc.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQK"},
			{"type":"input_text","text":"总结一下"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Contains(out, []byte("doc.pdf")) {
		t.Fatalf("二进制附件没有任何说明（会被当成不存在）: %s", out)
	}
}

// 超长附件必须截断并标注，不能原样内联（会直接顶爆上下文 → 11115）。
func TestResponsesRequestToChatInputFileTruncated(t *testing.T) {
	big := strings.Repeat("x", filePartMaxBytes+5000)
	dataURL := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(big))
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"message","role":"user","content":[{"type":"input_file","filename":"big.txt","file_data":"` + dataURL + `"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out) > filePartMaxBytes*2 {
		t.Fatalf("超长附件未截断：out=%d bytes", len(out))
	}
	if !bytes.Contains(out, []byte("已截断")) {
		t.Fatalf("截断未标注（模型会以为内容完整）: %s", out[len(out)-200:])
	}
}
// 工具结果里的 input_file 同样要内联（read_file 类工具回传文件）。
func TestResponsesRequestToChatToolOutputFile(t *testing.T) {
	dataURL := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("订单号 A12345"))
	raw := []byte(`{"model":"wb:glm-5.3-flash","input":[
		{"type":"function_call","call_id":"c1","name":"read_file","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_file","filename":"order.txt","file_data":"` + dataURL + `"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"读到了什么"}]}]}`)
	out, err := responsesRequestToChat(raw)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Contains(out, []byte("A12345")) {
		t.Fatalf("工具结果的附件未内联: %s", out)
	}
}
