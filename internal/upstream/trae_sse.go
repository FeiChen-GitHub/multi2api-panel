// trae_sse.go TRAE SOLO 自定义 SSE → OpenAI SSE（流式转换 + 非流式聚合）。
//
// SOLO 事件序列（实测，对照 trae2api/internal/upstream/solosse.go）：
//
//	event:metadata    data:{"model":"","session_id":"..."}
//	event:output      data:{"response":"<增量>","reasoning_content":"<思考增量>","tool_calls":null|...}  ×N
//	event:token_usage data:{"prompt_tokens":..,"completion_tokens":..,"total_tokens":..}
//	event:done        data:{"finish_reason":"stop"}
//	event:error       data:{"code":1005,"message":"..."}
//
// 转换策略：ChatClient 只回原始流，格式归一化在这里由 handler 侧调用 —— 与
// WorkBuddy 的「status + 可读流 + 错误分类」口径一致（DESIGN §6.1）。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TraeEvent 单条 SOLO SSE 事件（归一化）。
type TraeEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMessage string
}

// TraeStreamError 上游 SSE 流内的业务错误（event:error）。
type TraeStreamError struct {
	Code int64
	Msg  string
}

func (e *TraeStreamError) Error() string {
	return fmt.Sprintf("trae solo error code=%d msg=%s", e.Code, e.Msg)
}

// Kind 把 SOLO 流内业务码映射到本项目统一的 ErrKind（DESIGN §6.4）。
// 1005/4008 = 权益/配额不足 → 硬冷却；4011 = 限流 → 软冷却；其余按客户端错误。
func (e *TraeStreamError) Kind() ErrKind {
	switch e.Code {
	case 1005, 4008:
		return ErrHardCredit
	case 4011:
		return ErrSoftRate
	default:
		return ErrClient
	}
}

// traeParseSOLOLine 解析一条事件（eventName 为 event 行值，dataLine 为 data 行值）。
func traeParseSOLOLine(eventName, dataLine string) (*TraeEvent, error) {
	ev := &TraeEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev, nil
}

// traeSSEState 维护一行 SSE 的 event/data 跨行累积。
type traeSSEState struct {
	event string
	data  strings.Builder
}

func (s *traeSSEState) reset() {
	s.event = ""
	s.data.Reset()
}

// traeScanLine 处理一行；事件边界（空行）时解析并返回事件。
func traeScanLine(st *traeSSEState, line string) *TraeEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := traeParseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// TraeAggregate 读取完整 SOLO 流，聚合为单个 OpenAI chat.completion（非流式用）。
func TraeAggregate(r io.Reader, model string) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		upstreamErr  error
	)
	st := &traeSSEState{}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev := traeScanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				content.WriteString(ev.Response)
				reasoning.WriteString(ev.Reasoning)
				traeMergeToolCallJSON(toolCalls, &toolOrder, ev.ToolCalls)
			case "token_usage":
				usage = ev.Usage
			case "done":
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
			case "error":
				upstreamErr = &TraeStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		traeSortInts(toolOrder)
		calls := make([]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// traeMergeToolCallJSON 把 SOLO output.tool_calls（null / 对象 / 数组）按 index 合并。
func traeMergeToolCallJSON(toolCalls map[int]map[string]any, toolOrder *[]int, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]any{one}
	}
	for _, call := range arr {
		if call == nil {
			continue
		}
		idx := 0
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		merged, seen := toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			toolCalls[idx] = merged
			*toolOrder = append(*toolOrder, idx)
		}
		traeMergeToolCallDelta(merged, call)
	}
}

// traeMergeToolCallDelta 合并流式 tool_call 片段：id/type/function.name 直覆盖，
// function.arguments 拼接。上游用 function_call 字段（实测），输出统一成 OpenAI 的 function。
func traeMergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		df, _ = delta["function_call"].(map[string]any) // SOLO 专属字段名
	}
	if df == nil {
		return
	}
	// 清理 SOLO 专属字段，只保留标准 OpenAI function 结构（name/arguments）。
	delete(df, "namespace")
	delete(df, "partial_arguments")
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

func traeSortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// TraeStreamToOpenAI 把 SOLO 流式转换成 OpenAI SSE chunk，每 chunk flush，
// 保证至少一个 [DONE]。onErr 在遇到 event:error 时回调（供上层冷却账号），
// 错误信息同时注入 SSE 事件流。调用方负责已写好 status 200 与响应头。
func TraeStreamToOpenAI(w http.ResponseWriter, r io.Reader, model string, onErr func(*TraeStreamError)) error {
	return TraeStreamToOpenAIWithUsage(w, r, model, onErr, nil)
}

// TraeStreamToOpenAIWithUsage 在 TraeStreamToOpenAI 的基础上增加 onUsage 回调：
// 收到 token_usage 事件时把 usage 原样上报给上层（供统计），不影响客户端输出。
func TraeStreamToOpenAIWithUsage(w http.ResponseWriter, r io.Reader, model string, onErr func(*TraeStreamError), onUsage func(map[string]any)) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var pendingUsage map[string]any
	sawDone := false
	st := &traeSSEState{}

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

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if ev := traeScanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				delta := map[string]any{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					var tc []map[string]any
					if err := json.Unmarshal(ev.ToolCalls, &tc); err == nil {
						for _, call := range tc {
							if fc, ok := call["function_call"].(map[string]any); ok {
								call["function"] = fc
								delete(call, "function_call")
							}
							if fn, ok := call["function"].(map[string]any); ok {
								delete(fn, "namespace")
								delete(fn, "partial_arguments")
							}
						}
						delta["tool_calls"] = tc
					}
				}
				if len(delta) > 0 {
					if err := writeChunk(delta, ""); err != nil {
						return err
					}
				}
			case "token_usage":
				pendingUsage = ev.Usage
				if onUsage != nil {
					onUsage(ev.Usage)
				}
			case "done":
				if err := writeChunk(map[string]any{}, ev.FinishReason); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
			case "error":
				se := &TraeStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
				if onErr != nil {
					onErr(se)
				}
				msg := se.Error()
				rawMsg, _ := json.Marshal(msg)
				if _, err := io.WriteString(w, "event: error\ndata: "+string(rawMsg)+"\n\n"); err != nil {
					return err
				}
				if err := writeDONE(); err != nil {
					return err
				}
				sawDone = true
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
