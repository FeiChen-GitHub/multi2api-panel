package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
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
//
// 工具结果里的图片要**挪到最后一条 user 消息**（2026-10-18 实测，11115 修复的第二步）：
// 上游只对「最后一条消息」里的 image_url part 做图片编码，其余位置的图一律静默丢弃：
//   - 图在 role:tool 的 content 里 → prompt 只算 2950，模型答"看不到图"；
//   - 图在中间某条 user 消息里（后面还有消息）→ prompt 2921，同样看不到；
//   - 图在最后一条 user 消息里 → prompt 6001，模型能准确描述内容。
//
// 所以这里把 tool 结果拆成「文本留 tool + 图片暂存」，循环结束后统一挂到最后一条
// user 消息上（没有则补一条）。这样既不打断 tool_call↔tool 的相邻配对（11148 判据），
// 又让图片真正被上游编码——token 按图片口径计（约 1k），而不是按 base64 文本
// 分词（2.4MB ≈ 2M token，正是 11115 的成因）。
func responsesInputToMessages(input any) ([]map[string]any, error) {
	switch v := input.(type) {
	case nil:
		return nil, &responsesError{400, "invalid_request", "input is required"}
	case string:
		return []map[string]any{{"role": "user", "content": v}}, nil
	case []any:
		var msgs []map[string]any
		// pendingImages 工具结果里的图片（见函数头注释），循环结束后统一挂到末条 user。
		var pendingImages []map[string]any
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
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": str(item["call_id"]),
					"content":      responsesToolOutputText(out),
				})
				pendingImages = append(pendingImages, responsesToolOutputImages(out)...)
			default:
				// 静默丢弃会表现为「模型失忆」，比报错难查。
				return nil, &responsesError{422, "unsupported_input_item",
					fmt.Sprintf("unsupported input item type %q", typ)}
			}
		}
		if len(pendingImages) > 0 {
			msgs = attachImagesToLastUser(msgs, dedupeImages(pendingImages))
		}
		return msgs, nil
	default:
		return nil, &responsesError{400, "invalid_request", "input must be a string or an array"}
	}
}

// attachImagesToLastUser 把图片 part 挂到最后一条 user 消息上（上游只编码末条消息的图）。
//
// 末条是 user：
//   - content 是数组 → 追加 image part；
//   - content 是字符串 → 改写成 [text, image...] parts；
//   - content 为空 → 直接变成 image parts。
//
// 末条不是 user（assistant/tool）→ 追加一条带说明文本的 user 消息。
// 说明文本只在该消息原本没有正文时补，避免改动客户端原话。
func attachImagesToLastUser(msgs []map[string]any, images []map[string]any) []map[string]any {
	const placeholder = "（工具返回的图片）"
	if n := len(msgs); n > 0 {
		last := msgs[n-1]
		if role, _ := last["role"].(string); role == "user" {
			switch c := last["content"].(type) {
			case []map[string]any:
				last["content"] = append(c, images...)
				return msgs
			case []any:
				last["content"] = append(c, toAnySlice(images)...)
				return msgs
			case string:
				parts := make([]any, 0, len(images)+1)
				if strings.TrimSpace(c) == "" {
					parts = append(parts, map[string]any{"type": "text", "text": placeholder})
				} else {
					parts = append(parts, map[string]any{"type": "text", "text": c})
				}
				parts = append(parts, toAnySlice(images)...)
				last["content"] = parts
				return msgs
			default:
				parts := make([]any, 0, len(images)+1)
				parts = append(parts, map[string]any{"type": "text", "text": placeholder})
				parts = append(parts, toAnySlice(images)...)
				last["content"] = parts
				return msgs
			}
		}
	}
	parts := make([]any, 0, len(images)+1)
	parts = append(parts, map[string]any{"type": "text", "text": placeholder})
	parts = append(parts, toAnySlice(images)...)
	return append(msgs, map[string]any{"role": "user", "content": parts})
}

// toAnySlice 把 []map[string]any 转成 []any（content parts 的统一表示）。
func toAnySlice(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
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
			case "input_file":
				// 附件（Codex 的「添加文件」/ 粘贴长文本）。上游 chat 协议没有文件 part，
				// 原样透传会被丢弃（模型答"附件呢？"），所以这里转成文本内联。
				if t, ok := filePartText(pm); ok && t != "" {
					texts = append(texts, t)
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
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

// responsesToolOutputText 提取 function_call_output.output 里的**文本**部分。
//
// 2026-10-18 实案（11115 prompt is too long: 2059849 > 1048576）：
// Codex 的 view_image / 截图类工具把图片以 Responses 的 content part 数组回传：
//
//	"output": [{"type":"input_image","image_url":"data:image/png;base64,<2.4MB>"}]
//
// 旧实现只认 {"text": ...} 对象形态，数组一律 json.Marshal 成字符串塞进
// role:tool 的 content——2.4MB base64 以纯文本出站，按 base64 分词约 1.2 字符/token
// 折算 ≈ 2.05M token，一张截图就把 1M 上下文顶爆（上游 400 code=11115）。
// 同一张图走 image_url part 时上游只算 ~1k token（实测 730KB 图：part 形态 2884，
// 文本形态 530206，差 180 倍）。
//
// 图片 part 不在这里返回：上游对 role:tool 的 image_url part 静默丢弃
// （实测 tool content 带图 prompt 只算 2950，模型回答"看不到图"），
// 必须由 responsesToolOutputImages 抽出、由调用方追加到 user 消息。
func responsesToolOutputText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		// 裸 data URL 字符串（部分客户端不包 part）：不是文本，交给图片通道。
		if _, ok := imageDataURL(c); ok {
			return ""
		}
		return c
	case map[string]any:
		if t, ok := c["text"].(string); ok {
			return t
		}
		if _, ok := imageDataURL(str(c["image_url"])); ok {
			return ""
		}
		if u, ok := c["image_url"].(map[string]any); ok {
			if _, ok := imageDataURL(str(u["url"])); ok {
				return ""
			}
		}
		return marshalText(c)
	case []any:
		var texts []string
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "input_text", "output_text", "text":
				texts = append(texts, str(pm["text"]))
			case "input_file":
				// 工具返回附件（如 read_file 回传 PDF/文本文件）同样内联，
				// 否则 role:tool 的 file part 会被上游丢弃，模型看不到。
				if t, ok := filePartText(pm); ok && t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n")
	default:
		return marshalText(c)
	}
}

// responsesToolOutputImages 抽出 function_call_output.output 里的图片 part
// （转成 chat 的 image_url part）。无图片 → nil。
//
// 上游只在 user 消息的 content part 里编码图片（实测），所以这些 part 由
// responsesInputToMessages 追加到一条紧随该批工具结果的 user 消息上。
func responsesToolOutputImages(v any) []map[string]any {
	var parts []map[string]any
	add := func(url string) {
		if url == "" {
			return
		}
		parts = append(parts, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": url},
		})
	}
	switch c := v.(type) {
	case string:
		if url, ok := imageDataURL(c); ok {
			add(url)
		}
	case map[string]any:
		add(imageURLOf(c))
	case []any:
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "input_image", "image_url":
				add(imageURLOf(pm))
			}
		}
	}
	return parts
}

// imageURLOf 从 part 取图片 URL（字符串形态或 {"url":...} 对象形态）。
func imageURLOf(part map[string]any) string {
	if u, ok := part["image_url"].(map[string]any); ok {
		return str(u["url"])
	}
	return str(part["image_url"])
}

// marshalText 把非文本形态兜底序列化成字符串（仅用于诊断，不承载图片数据）。
func marshalText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// imageDataURL 判定字符串是否为图片 data URL（data:image/...;base64,...）。
// 只认前缀与 base64 标记，不校验负载——负载合法性交给上游（与 image_url 同口径：
// 网关不做图片解码，坏图由上游返回真实错误）。
func imageDataURL(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if len(t) < len("data:image/") || !strings.HasPrefix(t, "data:image/") {
		return "", false
	}
	if !strings.Contains(t[:min(len(t), 64)], ";base64,") {
		return "", false
	}
	return t, true
}

// dedupeImages 按 URL 去重（同一张截图可能在历史里出现多次）。
// 每张图上游按 ~1-2k token 计，重复挂载纯属浪费；去重后顺序保持首次出现。
func dedupeImages(in []map[string]any) []map[string]any {
	seen := make(map[string]bool, len(in))
	out := make([]map[string]any, 0, len(in))
	for _, p := range in {
		url := ""
		if m, ok := p["image_url"].(map[string]any); ok {
			url, _ = m["url"].(string)
		}
		if url != "" && seen[url] {
			continue
		}
		if url != "" {
			seen[url] = true
		}
		out = append(out, p)
	}
	return out
}
// filePartMaxBytes 单个 input_file 内联成文本的字节上限。
// 上游上下文有限（最小 100K token ≈ 300KB 文本），无上限内联会让一张大附件直接
// 顶爆 11115。超限截断并在文末标注，让模型知道内容不完整（不静默丢）。
const filePartMaxBytes = 256 << 10

// filePartText 把 Responses 的 input_file part 转成可内联的文本。
//
// 支持的形态：
//   - file_data 是 data:text/...;base64,...（客户端内联附件）→ 解码成文本；
//   - file_data 是 data:application/json 等同理；
//   - 二进制（application/pdf、图片等）或只有 file_id / file_url（需要额外下载）
//     → 返回一段说明文本，明确告知模型「附件存在但网关无法读取」，
//     而不是静默丢弃（静默丢弃会表现为「附件呢？」「模型失忆」）。
func filePartText(part map[string]any) (string, bool) {
	name := str(part["filename"])
	if name == "" {
		name = "attachment"
	}
	data := str(part["file_data"])
	if data == "" {
		// file_id / file_url 需要上游文件接口才能取，本网关不代理文件存储。
		if str(part["file_id"]) != "" || str(part["file_url"]) != "" {
			return "[附件 " + name + "：网关无法读取该文件引用，请让用户改用粘贴文本或本地路径]", true
		}
		return "", false
	}
	mime, payload, ok := splitDataURL(data)
	if !ok {
		// 非 data URL：当作纯文本原样内联（部分客户端直接塞文件内容）。
		return truncateText(data, name), true
	}
	if !isTextMIME(mime) {
		return "[附件 " + name + "（" + mime + "）：二进制附件无法转换为文本，请改用支持该格式的工具或粘贴内容]", true
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "[附件 " + name + "：base64 解码失败]", true
	}
	return "[附件 " + name + " 的内容如下]\n" + truncateText(string(raw), name), true
}

// splitDataURL 拆分 data URL 的 mime 与 base64 负载。
func splitDataURL(s string) (mime, payload string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := s[len("data:"):comma]
	if !strings.Contains(meta, ";base64") {
		return "", "", false
	}
	mime = strings.TrimSuffix(strings.SplitN(meta, ";", 2)[0], ";")
	if mime == "" {
		mime = "application/octet-stream"
	}
	return mime, s[comma+1:], true
}

// isTextMIME 判定该 MIME 是否可按文本内联。
func isTextMIME(mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if strings.HasPrefix(m, "text/") {
		return true
	}
	switch m {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml",
		"application/javascript", "application/x-sh", "application/sql",
		"application/toml", "application/x-ndjson":
		return true
	}
	return strings.HasSuffix(m, "+json") || strings.HasSuffix(m, "+xml")
}

// truncateText 超过 filePartMaxBytes 时截断并标注（按 UTF-8 边界切，避免半个字符）。
func truncateText(s, name string) string {
	if len(s) <= filePartMaxBytes {
		return s
	}
	cut := filePartMaxBytes
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n\n[附件 " + name + " 内容过长，已截断（原始 " +
		strconv.Itoa(len(s)) + " 字节，保留前 " + strconv.Itoa(cut) + " 字节）]"
}
