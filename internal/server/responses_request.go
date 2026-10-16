package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// responsesError 客户端可见的转换错误：Status 直接决定 HTTP 码。
type responsesError struct {
	Status  int
	Code    string
	Message string
}

func (e *responsesError) Error() string { return e.Message }

// responsesRequestToChat 把 Responses 请求体转成内部 chat 形状（spec §8）。
// 纯函数：不碰上游、不读全局状态，因此可在任意阶段调用。
// 顶层未知字段一律忽略（OpenAI 客户端会发大量可选字段）。
func responsesRequestToChat(raw []byte) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, &responsesError{400, "invalid_request", "invalid JSON body: " + err.Error()}
	}

	if v, ok := in["previous_response_id"]; ok && v != nil {
		if s, _ := v.(string); s != "" {
			return nil, &responsesError{400, "unsupported_parameter",
				"previous_response_id is not supported: this gateway is stateless; send the full conversation in input"}
		}
	}

	messages, err := responsesInputToMessages(in["input"])
	if err != nil {
		return nil, err
	}
	if s, _ := in["instructions"].(string); strings.TrimSpace(s) != "" {
		messages = append([]map[string]any{{"role": "system", "content": s}}, messages...)
	}

	out := map[string]any{"messages": messages}
	copyIfSet(in, out, "model", "stream", "tool_choice", "temperature", "top_p",
		"prompt_cache_key", "metadata", "user", "logprobs", "top_logprobs")
	if rc, ok := in["reasoning"].(map[string]any); ok {
		if e, _ := rc["effort"].(string); e != "" {
			out["reasoning_effort"] = e
		}
	}
	if v, ok := in["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if tools, err := responsesToolsToChat(in["tools"]); err != nil {
		return nil, err
	} else if tools != nil {
		// tools 缺失时不写该键：ToolNameAllowlist 靠它判断是否启用 DSML 修复。
		out["tools"] = tools
	}
	return json.Marshal(out)
}

func copyIfSet(in, out map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := in[k]; ok && v != nil {
			out[k] = v
		}
	}
}

// responsesInputToMessages 按 input 出现顺序产出 chat messages。
// 连续的 function_call 合并进同一条 assistant 消息的 tool_calls。
func responsesInputToMessages(input any) ([]map[string]any, error) {
	switch v := input.(type) {
	case nil:
		return nil, &responsesError{400, "invalid_request", "input is required"}
	case string:
		return []map[string]any{{"role": "user", "content": v}}, nil
	case []any:
		var msgs []map[string]any
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, &responsesError{400, "invalid_request", "input items must be objects"}
			}
			typ, _ := item["type"].(string)
			switch typ {
			case "reasoning":
				// 我们不产加密推理块，回传的推理内容无法复用，丢弃。
			case "message", "":
				// type 省略/为空按消息处理：OpenAI Responses 的 EasyInputMessage 里
				// type 是可选字段（role+content 即一条消息）。若干客户端（含 Codex
				// 的部分版本）发消息项时不带 type，套用严格白名单会把正常请求整条
				// 422 掉（实测报 unsupported input item type ""）。
				msg, err := responsesMessageItem(item)
				if err != nil {
					return nil, err
				}
				msgs = append(msgs, msg)
			case "function_call":
				call := map[string]any{
					"id":   str(item["call_id"]),
					"type": "function",
					"function": map[string]any{
						"name":      str(item["name"]),
						"arguments": str(item["arguments"]),
					},
				}
				if n := len(msgs); n > 0 && msgs[n-1]["role"] == "assistant" {
					if tcs, ok := msgs[n-1]["tool_calls"].([]map[string]any); ok {
						msgs[n-1]["tool_calls"] = append(tcs, call)
						continue
					}
				}
				msgs = append(msgs, map[string]any{
					"role": "assistant", "tool_calls": []map[string]any{call},
				})
			case "function_call_output":
				out := item["output"]
				if m, ok := out.(map[string]any); ok {
					out = m["text"]
				}
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": str(item["call_id"]),
					"content":      contentText(out),
				})
			default:
				// 静默丢弃会表现为「模型失忆」，比报错难查。
				return nil, &responsesError{422, "unsupported_input_item",
					fmt.Sprintf("unsupported input item type %q", typ)}
			}
		}
		return msgs, nil
	default:
		return nil, &responsesError{400, "invalid_request", "input must be a string or an array"}
	}
}

func responsesMessageItem(item map[string]any) (map[string]any, error) {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	switch content := item["content"].(type) {
	case string:
		return map[string]any{"role": role, "content": content}, nil
	case []any:
		parts := make([]map[string]any, 0, len(content))
		var texts []string
		for _, p := range content {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "input_text", "output_text":
				t := str(pm["text"])
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			case "input_image":
				url := str(pm["image_url"])
				if u, ok := pm["image_url"].(map[string]any); ok {
					url = str(u["url"])
				}
				if url == "" {
					continue
				}
				// 现有 chat 路径已支持图片 part。
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": url},
				})
			}
		}
		// 纯文本走 content 字符串：既有 chat 路径与 DSML 修复都按字符串处理。
		if len(parts) == 0 {
			return map[string]any{"role": role, "content": strings.Join(texts, "\n")}, nil
		}
		if len(texts) == len(parts) {
			return map[string]any{"role": role, "content": strings.Join(texts, "\n")}, nil
		}
		return map[string]any{"role": role, "content": parts}, nil
	case nil:
		return map[string]any{"role": role, "content": ""}, nil
	default:
		return nil, &responsesError{400, "invalid_request", "message content must be a string or an array"}
	}
}

// responsesToolsToChat 把 Responses 的工具形态转成 chat 的嵌套形态。
//
// 三种形态：
//   - {"type":"function", name, description, parameters} → chat function（原形态）
//   - {"type":"namespace", name, tools:[...]}：容器形态（OpenAI 官方 tool_namespace，
//     Codex CLI 用它装 MCP 工具组）。内部 function **保持原名提升到顶层**——
//     namespace 是分组元数据而非工具名的一部分（上游 Trae 的 SSE 也把它当元数据
//     删掉，见 trae_sse.go 的 delete(df,"namespace")），所以既不能拼前缀，
//     也不能整组丢弃。
//   - 其余托管工具（tool_search / web_search / mcp / computer / custom / shell 等）：
//     本网关不实现 → **跳过并记日志**，而不是报错。
//
// 为什么第三类从 422 改成跳过：Responses 客户端（Codex CLI）把托管工具和业务工具
// 混在同一个 tools 数组里发，只要有一个不认识就整个请求 422，结果是客户端彻底
// 不可用（实测报错 "unsupported tool type \"namespace\""）。丢掉一项托管能力的
// 代价，远小于整条链路报废。
func responsesToolsToChat(tools any) ([]map[string]any, error) {
	if tools == nil {
		return nil, nil
	}
	list, ok := tools.([]any)
	if !ok {
		return nil, &responsesError{400, "invalid_request", "tools must be an array"}
	}
	out := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		t, ok := raw.(map[string]any)
		if !ok {
			return nil, &responsesError{400, "invalid_request", "tools items must be objects"}
		}
		appendResponsesTool(&out, t, 0)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// appendResponsesTool 展开单个工具条目并追加到 out。depth 用于防御嵌套 namespace
// （规范禁止，但不信任客户端输入：真遇到就跳过而不是递归爆栈）。
func appendResponsesTool(out *[]map[string]any, t map[string]any, depth int) {
	typ, _ := t["type"].(string)
	switch typ {
	case "", "function":
		fn := map[string]any{"name": str(t["name"])}
		if d, ok := t["description"]; ok {
			fn["description"] = d
		}
		if p, ok := t["parameters"]; ok {
			fn["parameters"] = p
		}
		*out = append(*out, map[string]any{"type": "function", "function": fn})
	case "namespace":
		if depth > 0 {
			log.Printf("WARN: [responses] 跳过嵌套 namespace 工具 %q（规范要求 namespace 内只能是 function/custom）", str(t["name"]))
			return
		}
		inner, _ := t["tools"].([]any)
		for _, raw := range inner {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			appendResponsesTool(out, it, depth+1)
		}
	default:
		// 不静默：留一条日志，便于事后定位"客户端发了什么、我们丢了什么"。
		log.Printf("WARN: [responses] 忽略不支持的工具 type=%q name=%q（本网关不实现，请求继续）", typ, str(t["name"]))
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func contentText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	default:
		b, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
