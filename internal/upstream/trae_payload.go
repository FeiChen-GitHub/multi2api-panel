// trae_payload.go OpenAI → TRAE SOLO llm_utils_chat 请求体改写。
//
// 对照 trae2api/internal/upstream/payload.go（已核实，规则来自实测）。与 WorkBuddy 的
// PrepareBodyOpt 是两套独立改写，互不复用：SOLO 有 config_name / function / tool_call
// 字段形态差异（tools[].function.parameters 上游是 string 而非 object）。
package upstream

import (
	"encoding/json"
	"strings"
)

const (
	// TraeFunction SOLO 请求固定 function 值（upstream 常量 Function）。
	TraeFunction = "solo_work_lite"
	// TraeDefaultModel SOLO 默认模型（trae2api DefaultConfigName，实测可用）。
	TraeDefaultModel = "glm-5.2"
)

// TraePrepareBody 单 pass 改写；无法解析时原样返回。
//
//	OpenAI: {model, messages, stream, tools, tool_choice, ...}
//	SOLO:   {messages, function:"solo_work_lite", stream:true,
//	         config_name:<model>, model:<model>}
func TraePrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = TraeFunction

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)

			// assistant 历史里的 tool_calls：OpenAI function → 上游 function_call。
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						// 上游要求 FunctionCall.Name 必填：无 name 的条目剔除，
						// 否则上游反序列化失败（11133 同源问题）。
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}

			if !present || content == nil {
				continue // 无 content 的消息（如纯 tool_calls assistant）保留字段但跳过改写
			}
			if c, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": c}}
			}
			// 已是数组 → 透传（多模态保守透传，未实测）
		}
	}

	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = TraeDefaultModel
	}
	obj["config_name"] = model
	obj["model"] = model

	traeNormalizeToolChoice(obj)
	traeNormalizeTools(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// traeNormalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none" / {"type":"none"} → 删 tool_choice + 删 tools/functions
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func traeNormalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// traeNormalizeTools 把 OpenAI tools 转为 SOLO 上游格式：parameters 对象序列化为
// JSON 字符串（上游 struct 该字段是 string），缺 function 的条目整体剔除。
func traeNormalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
