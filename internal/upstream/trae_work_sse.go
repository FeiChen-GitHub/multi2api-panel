// trae_work_sse.go TRAE Work 通道下行事件流 → OpenAI（流式 + 非流式聚合）。
//
// Work 的事件形状与 SOLO **不同**（对照 trae2api work_client.go 的
// StreamWorkToOpenAI / AggregateWork，已核实）：
//
//	SOLO: data:{"response":"<增量>","reasoning_content":"...","tool_calls":...}
//	Work: data:{"event":"plan_item","payload":{"thought":"...","reasoning_content":"..."}}
//	      data:{"event":"output","payload":{"choices":[{"text":"<累积全文>"}]}}
//	      data:{"event":"token_usage","payload":{...}}
//	      data:{"event":"done","payload":{"last_assistant_response":"..."}}
//
// 两个关键差异：
//  1. Work 的字段在 payload 里（顶层也可能有 event/code/message）；
//  2. **output 的 text 是累积全文不是增量**——必须自己做差分，否则客户端
//     每帧都收到从头开始的全文，输出直接炸开成 N 倍重复（trae2api 的
//     handleDelta 就是干这个的）。
//
// 之前 Work 走的是 SOLO 解析器，两种形状不匹配 → Work 响应体全空。本文件把
// Work 分支独立出来，SOLO 解析器不动（零回归）。
package upstream

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// traeWorkEvent Work 下行事件（归一化）。
type traeWorkEvent struct {
	Event     string          // 顶层 event，或 data.event，或 SSE event 行
	Code      int64           // error 事件的业务码
	Message   string          // error 事件的文案
	Payload   json.RawMessage // 事件负载（顶层 payload 或 data.payload）
	IsOpenAI  bool            // 已是标准 OpenAI chunk（透传）
	OpenAIRaw string          // 透传原文
	Done      bool            // 已是 [DONE]
}

// traeParseWorkLine 解析一条 Work data 行。非 JSON（如 [DONE]）返回 nil,false。
func traeParseWorkLine(dataLine string, currentEvent string) (*traeWorkEvent, bool) {
	s := strings.TrimSpace(dataLine)
	if s == "" {
		return nil, false
	}
	if s == "[DONE]" {
		return &traeWorkEvent{Done: true}, true
	}
	if strings.Contains(s, `"chat.completion.chunk"`) {
		// 上游（或桥接）已给标准 OpenAI chunk：原样透传，不再解析。
		return &traeWorkEvent{IsOpenAI: true, OpenAIRaw: s}, true
	}
	var obj struct {
		Event   string          `json:"event"`
		Code    *float64        `json:"code"`
		Message string          `json:"message"`
		Payload json.RawMessage `json:"payload"`
		Data    struct {
			Event   string          `json:"event"`
			Payload json.RawMessage `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, false
	}
	ev := &traeWorkEvent{Event: obj.Event, Message: obj.Message, Payload: obj.Payload}
	if ev.Event == "" {
		ev.Event = obj.Data.Event
	}
	if len(ev.Payload) == 0 {
		ev.Payload = obj.Data.Payload
	}
	if ev.Event == "" {
		ev.Event = currentEvent // 回落到 SSE event 行（部分帧只给 event:）
	}
	if obj.Code != nil {
		ev.Code = int64(*obj.Code)
	}
	return ev, true
}

// traeWorkTextOf 取一个 Work 事件里的正文增量候选（按优先级）：
// output.choices[].text（累积全文）、done.last_assistant_response、
// plan_item.thought / reasoning_content（思考增量）。
// 返回 isFull=true 表示这是累积全文，需要调用方做差分。
func traeWorkTextOf(ev *traeWorkEvent) (text string, isFull bool) {
	if len(ev.Payload) == 0 {
		return "", false
	}
	switch ev.Event {
	case "output":
		var out struct {
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
		}
		if json.Unmarshal(ev.Payload, &out) != nil {
			return "", false
		}
		for _, c := range out.Choices {
			// "[]" 是上游的空数组占位，不是内容。
			if c.Text != "" && c.Text != "[]" {
				return c.Text, true
			}
		}
	case "done":
		var dp struct {
			LastAssistantResponse string `json:"last_assistant_response"`
		}
		if json.Unmarshal(ev.Payload, &dp) != nil || dp.LastAssistantResponse == "" {
			return "", false
		}
		// 上游给的是 JSON 数组字符串（["..."]）：取首项。
		var arr []string
		if json.Unmarshal([]byte(dp.LastAssistantResponse), &arr) == nil && len(arr) > 0 {
			return arr[0], true
		}
		return dp.LastAssistantResponse, true
	case "plan_item":
		var plan struct {
			Thought          string `json:"thought"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCallInfo     struct {
				Params struct {
					Summary string `json:"summary"`
				} `json:"params"`
			} `json:"tool_call_info"`
		}
		if json.Unmarshal(ev.Payload, &plan) != nil {
			return "", false
		}
		if plan.ReasoningContent != "" {
			return plan.ReasoningContent, false
		}
		if plan.Thought != "" {
			return plan.Thought, false
		}
		return plan.ToolCallInfo.Params.Summary, false
	}
	return "", false
}

// traeWorkUsageOf 取 token_usage 事件的 usage 对象（原样透出）。
func traeWorkUsageOf(ev *traeWorkEvent) map[string]any {
	if ev.Event != "token_usage" || len(ev.Payload) == 0 {
		return nil
	}
	var u map[string]any
	if json.Unmarshal(ev.Payload, &u) != nil {
		return nil
	}
	return u
}

// traeWorkDelta 把「累积全文」差分成增量（trae2api handleDelta 同口径）：
//   - 新文本以已见全文为前缀 → 取后缀作增量；
//   - 新文本不是已见全文的前缀、也未被包含 → 视为全新一段，整段作增量；
//   - 其余（重复/乱序）→ 无增量。
//
// full 是指针：调用方持有累积状态，跨帧共享。
func traeWorkDelta(full *string, text string) string {
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, *full) && len(text) > len(*full) {
		d := text[len(*full):]
		*full = text
		return d
	}
	if !strings.Contains(*full, text) {
		*full += text
		return text
	}
	return ""
}

// TraeStreamWorkToOpenAI 把 Work 下行流转换成 OpenAI SSE chunk，每 chunk flush。
// 事件序列结束（done / [DONE] / EOF）保证写且仅写一个 [DONE]。
// onErr 在遇到 error 事件时回调（供上层冷却该账号的 Work 通道）。
// 调用方负责已写好 status 200 与响应头。
func TraeStreamWorkToOpenAI(w http.ResponseWriter, r io.Reader, model string, onErr func(*TraeStreamError)) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(r, 64*1024)
	id := "chatcmpl-" + traeNewUUID()
	var pendingUsage map[string]any
	// 正文与思考各用一条累积基线：混用一条的话，思考文本会插在正文的差分序列里，
	// 后续正文帧不再以基线为前缀 → 差分退化成「整段重复下发」。
	var fullText, fullReasoning string
	sawDone := false

	writeChunk := func(delta map[string]any, finish string) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{choice},
		}
		if pendingUsage != nil {
			chunk["usage"] = pendingUsage
			pendingUsage = nil
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeDONE := func() error {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	currentEvent := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			ev, ok := traeParseWorkLine(strings.TrimPrefix(trimmed, "data:"), currentEvent)
			if !ok {
				break
			}
			switch {
			case ev.Done:
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			case ev.IsOpenAI:
				if _, err := io.WriteString(w, "data: "+ev.OpenAIRaw+"\n\n"); err != nil {
					return err
				}
				if fl != nil {
					fl.Flush()
				}
			case ev.Event == "error":
				se := &TraeStreamError{Code: ev.Code, Msg: ev.Message}
				if onErr != nil {
					onErr(se)
				}
				rawMsg, _ := json.Marshal(se.Error())
				if _, err := io.WriteString(w, "event: error\ndata: "+string(rawMsg)+"\n\n"); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			case ev.Event == "done":
				// done 可能携带尾部正文（last_assistant_response）：先补增量再收尾。
				if text, _ := traeWorkTextOf(ev); text != "" {
					// 一律差分：done 携带的是累积全文（"answer!"），已下发过的
					// 部分（"answer"）必须裁掉，否则客户端收到重复的正文。
					d := traeWorkDelta(&fullText, text)
					if d != "" {
						if err := writeChunk(map[string]any{"content": d}, ""); err != nil {
							return err
						}
					}
				}
				if err := writeChunk(map[string]any{}, "stop"); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			default:
				if u := traeWorkUsageOf(ev); u != nil {
					pendingUsage = u
				}
				text, _ := traeWorkTextOf(ev)
				if text == "" {
					break
				}
				// output 给累积全文、plan_item 给思考增量，但两者都可能重复推送
				// 已下发过的内容 —— 统一差分，重复部分自然被裁掉。
				// plan_item 走 reasoning_content（思考过程），output 走 content；
				// 两条序列各自差分，互不干扰。
				key := "content"
				d := text
				if ev.Event == "plan_item" {
					key = "reasoning_content"
					d = traeWorkDelta(&fullReasoning, text)
				} else {
					d = traeWorkDelta(&fullText, text)
				}
				if d == "" {
					break
				}
				if err := writeChunk(map[string]any{key: d}, ""); err != nil {
					return err
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if !sawDone {
		// 幂等兜底：上游中断（无 done）仍写 [DONE]。
		return writeDONE()
	}
	return nil
}

// TraeAggregateWork 把完整 Work 流聚合成单个 OpenAI chat.completion（非流式用）。
// 流内 error 事件 → 返回 *TraeStreamError（调用方据此冷却 Work 通道）。
func TraeAggregateWork(r io.Reader, model string) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content strings.Builder
		// 与流式同口径：正文与思考各一条差分基线，混用会破坏前缀匹配。
		fullText, fullReasoning string
		usage                   map[string]any
		upErr                   error
	)
	currentEvent := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		} else if strings.HasPrefix(trimmed, "data:") {
			ev, ok := traeParseWorkLine(strings.TrimPrefix(trimmed, "data:"), currentEvent)
			if ok {
				if ev.Event == "error" {
					upErr = &TraeStreamError{Code: ev.Code, Msg: ev.Message}
				} else if u := traeWorkUsageOf(ev); u != nil {
					usage = u
				} else if text, _ := traeWorkTextOf(ev); text != "" {
					// plan_item（思考）并入正文（非流式无独立 reasoning 字段语义，
					// trae2api 的 AggregateWork 同样只累积正文），但各自差分。
					if ev.Event == "plan_item" {
						content.WriteString(traeWorkDelta(&fullReasoning, text))
					} else {
						content.WriteString(traeWorkDelta(&fullText, text))
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upErr != nil {
		return nil, upErr
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + traeNewUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content.String()},
				"finish_reason": "stop",
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}
