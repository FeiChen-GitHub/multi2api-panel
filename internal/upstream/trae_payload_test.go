package upstream

import (
	"encoding/json"
	"testing"
)

// Trae SOLO 的请求体改写：OpenAI 形状 → llm_utils_chat 形状。
// 规则对照 trae2api/internal/upstream/payload.go（已核实）。
func TestTraePrepareBodyRewritesForSOLO(t *testing.T) {
	in := `{"model":"glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}],` +
		`"tool_choice":{"type":"auto"}}`
	out := TraePrepareBody([]byte(in))

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true（SOLO 只支持流式，非流式由服务端聚合）", got["stream"])
	}
	if got["function"] != TraeFunction {
		t.Fatalf("function = %v, want %q", got["function"], TraeFunction)
	}
	if got["config_name"] != "glm-5.2" || got["model"] != "glm-5.2" {
		t.Fatalf("config_name/model = %v/%v, want glm-5.2", got["config_name"], got["model"])
	}

	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v", got["messages"])
	}
	content, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want [{type:text,text:hi}]", msgs[0].(map[string]any)["content"])
	}
	part := content[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "hi" {
		t.Fatalf("content part = %#v", part)
	}

	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", got["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if _, isString := fn["parameters"].(string); !isString {
		t.Fatalf("tools[].function.parameters 必须是 JSON 字符串（上游 Go struct 如此），got %T", fn["parameters"])
	}
	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want \"auto\"", got["tool_choice"])
	}
}

// model 缺失/为空时回落 Trae 默认模型（glm-5.2）。
func TestTraePrepareBodyDefaultsModel(t *testing.T) {
	out := TraePrepareBody([]byte(`{"messages":[]}`))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != TraeDefaultModel || got["config_name"] != TraeDefaultModel {
		t.Fatalf("model/config_name = %v/%v, want %q", got["model"], got["config_name"], TraeDefaultModel)
	}
}

// D7：`auto` 是别名不是模型名，必须映射到默认模型（glm-5.2）。
// 直接当 config_name 上发会被上游拒，整个请求失败。
func TestTraePrepareBodyMapsAutoAlias(t *testing.T) {
	for _, in := range []string{`{"model":"auto","messages":[]}`, `{"model":"AUTO","messages":[]}`} {
		out := TraePrepareBody([]byte(in))
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, in)
		}
		if got["model"] != TraeDefaultModel || got["config_name"] != TraeDefaultModel {
			t.Fatalf("%s → model/config_name = %v/%v, want %q（auto 必须映射为默认模型）",
				in, got["model"], got["config_name"], TraeDefaultModel)
		}
	}
}

// assistant 历史里的 OpenAI tool_calls.function 要转成上游的 function_call；
// 缺 name 的条目上游会反序列化失败，必须整体剔除。
func TestTraePrepareBodyConvertsAssistantToolCalls(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},` +
		`{"id":"c2","type":"function","function":{"arguments":"{}"}}]}]}`
	out := TraePrepareBody([]byte(in))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %#v, want 1 条（缺 name 的被剔除）", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if _, hasFunction := tc["function"]; hasFunction {
		t.Fatalf("OpenAI 的 function 键应已删除: %#v", tc)
	}
	fc, ok := tc["function_call"].(map[string]any)
	if !ok || fc["name"] != "f" {
		t.Fatalf("function_call = %#v, want name=f", tc["function_call"])
	}
}

// tool_choice:"none" 必须把 tools/functions 一起删掉（上游按 string 类型解析）。
func TestTraePrepareBodyToolChoiceNoneDropsTools(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[],"tool_choice":"none",` +
		`"tools":[{"type":"function","function":{"name":"f"}}],"functions":[{"name":"f"}]}`
	out := TraePrepareBody([]byte(in))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["tool_choice"]; ok {
		t.Fatalf("tool_choice 应被删除: %#v", got["tool_choice"])
	}
	if _, ok := got["tools"]; ok {
		t.Fatalf("tools 应被删除: %#v", got["tools"])
	}
	if _, ok := got["functions"]; ok {
		t.Fatalf("functions 应被删除: %#v", got["functions"])
	}
}

// 非法 JSON 原样返回（不 panic、不伪造）。
func TestTraePrepareBodyInvalidJSONPassthrough(t *testing.T) {
	in := []byte(`not json`)
	out := TraePrepareBody(in)
	if string(out) != string(in) {
		t.Fatalf("invalid JSON should pass through unchanged, got %s", out)
	}
}
