package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseFrame / sseDone 拼一个 chat chunk SSE 帧（用 json.Marshal 避免手写转义出错）。
func sseFrame(id, content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": id,
		"choices": []any{map[string]any{
			"delta": map[string]any{"role": "assistant", "content": content},
		}},
	})
	return "data: " + string(b) + "\n\n"
}

func sseDone() string {
	return "data: [DONE]\n\n"
}

// 文本流：created → in_progress → output_item.added → content_part.added
// → delta* → done 链 → completed。
func TestResponsesWriterTextEventSequence(t *testing.T) {
	rr := httptest.NewRecorder()
	rw := NewResponsesWriter(rr, nil)
	write := func(s string) { _, _ = rw.Write([]byte(s)) }
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
	write("data: [DONE]\n\n")
	out := rr.Body.String()
	for _, ev := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(out, ev) {
			t.Errorf("缺事件 %q:\n%s", ev, out)
		}
	}
	if strings.Contains(out, "[DONE]") {
		t.Error("Responses 无 [DONE] 哨兵，必须吞掉")
	}
	if !strings.Contains(out, `"text":"你好"`) {
		t.Errorf("completed 未带累积文本:\n%s", out)
	}
	if !sequenceMonotonic(t, out) {
		t.Errorf("sequence_number 非严格递增:\n%s", out)
	}
}

// 工具：output_item.added(function_call) → arguments.delta* → arguments.done
// → output_item.done → completed；call_id 严格往返（Review Focus #3）。
func TestResponsesWriterToolCallSequence(t *testing.T) {
	rr := httptest.NewRecorder()
	rw := NewResponsesWriter(rr, nil)
	write := func(s string) { _, _ = rw.Write([]byte(s)) }
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"\"}}]}}]}\n\n")
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n")
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n")
	write("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	write("data: [DONE]\n\n")
	out := rr.Body.String()
	for _, ev := range []string{
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(out, ev) {
			t.Errorf("缺事件 %q:\n%s", ev, out)
		}
	}
	if !strings.Contains(out, `"call_id":"call_1"`) {
		t.Errorf("call_id 未透出:\n%s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"cmd\":\"ls\"}"`) {
		t.Errorf("arguments 未累积拼接:\n%s", out)
	}
}

// 上游 error 帧：只发 response.failed，不发 response.completed（Review Focus #4）。
func TestResponsesWriterErrorFrameNoCompleted(t *testing.T) {
	rr := httptest.NewRecorder()
	rw := NewResponsesWriter(rr, nil)
	write := func(s string) { _, _ = rw.Write([]byte(s)) }
	write("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	write("data: {\"error\":{\"message\":\"boom\",\"code\":6004}}\n\n")
	write("data: [DONE]\n\n")
	out := rr.Body.String()
	if !strings.Contains(out, "event: response.failed") {
		t.Errorf("缺 response.failed:\n%s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Errorf("error 帧后不得发 response.completed:\n%s", out)
	}
	if rw.LastErrorFrame() == "" {
		t.Error("LastErrorFrame 应返回 error 帧原文，供上层做错误分类")
	}
}

// Flusher 必须被转发（Review Focus #1）：没有 Flusher 流式会退化成「等结束才吐」。
func TestResponsesWriterForwardsFlusher(t *testing.T) {
	rr := httptest.NewRecorder()
	if _, ok := interface{}(NewResponsesWriter(rr, nil)).(interface{ Flush() }); !ok {
		t.Fatal("ResponsesWriter 必须实现 Flush()")
	}
}

// sequenceMonotonic 断言所有 data 帧的 sequence_number 严格递增。
func sequenceMonotonic(t *testing.T, sse string) bool {
	t.Helper()
	last := -1
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj) != nil {
			continue
		}
		s, ok := obj["sequence_number"].(float64)
		if !ok {
			t.Errorf("事件缺 sequence_number: %s", line)
			return false
		}
		n := int(s)
		if n <= last {
			t.Errorf("sequence_number 非递增: %d 之后 %d", last, n)
			return false
		}
		last = n
	}
	return true
}

// 断言 id 前缀（chat id → resp_）在流式与非流式一致。
func TestResponsesWriterIDPrefix(t *testing.T) {
	rr := httptest.NewRecorder()
	rw := NewResponsesWriter(rr, nil)
	_, _ = rw.Write([]byte(sseFrame("c1", "x")))
	_, _ = rw.Write([]byte(sseDone()))
	if !strings.Contains(rr.Body.String(), "resp_") {
		t.Fatalf("id 需 resp_ 前缀:\n%s", rr.Body.String())
	}
}

// 分帧写入（写入点逐帧 flush，字节常被拆开）不得丢内容。
func TestResponsesWriterHandlesSplitWrites(t *testing.T) {
	rr := httptest.NewRecorder()
	rw := NewResponsesWriter(rr, nil)
	full := sseFrame("c1", "好") + sseDone()
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		_, _ = rw.Write([]byte(full[i:end]))
	}
	out := rr.Body.String()
	if !strings.Contains(out, "event: response.created") || !strings.Contains(out, "event: response.completed") {
		t.Fatalf("分帧写入丢帧:\n%s", out)
	}
}

// reasoning + message + tool_call 共存时，output_index 必须三个互不相同，
// 且 item 的关闭顺序按 output_index 升序（客户端按 index 驱动 UI 生命周期）。
// 实测发现过 tool_call 与 message 同为 output_index 1 的缺陷。
func TestResponsesWriterOutputIndexUniqueAcrossItemKinds(t *testing.T) {
	rr := httptest.NewRecorder()
	rw := NewResponsesWriter(rr, nil)
	w := func(s string) { _, _ = rw.Write([]byte(s)) }
	w(`data: {"id":"c1","choices":[{"delta":{"role":"assistant","reasoning_content":"想一下"}}]}` + "\n\n")
	w(`data: {"id":"c1","choices":[{"delta":{"content":"好的"}}]}` + "\n\n")
	w(`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]}}]}` + "\n\n")
	w(`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	w(sseDone())

	out := rr.Body.String()
	var addedIdx []int
	var doneIdx []int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		idx, ok := ev["output_index"].(float64)
		if !ok {
			continue
		}
		switch ev["type"] {
		case "response.output_item.added":
			addedIdx = append(addedIdx, int(idx))
		case "response.output_item.done":
			doneIdx = append(doneIdx, int(idx))
		}
	}
	seen := map[int]bool{}
	for _, i := range addedIdx {
		if seen[i] {
			t.Fatalf("output_index %d 被两个 item 复用:\n%s", i, out)
		}
		seen[i] = true
	}
	if len(addedIdx) != 3 {
		t.Fatalf("added 的 item 数 = %d, want 3 (reasoning/message/function_call):\n%s", len(addedIdx), out)
	}
	for i := 1; i < len(doneIdx); i++ {
		if doneIdx[i] <= doneIdx[i-1] {
			t.Fatalf("output_item.done 未按 output_index 升序: %v:\n%s", doneIdx, out)
		}
	}
}
