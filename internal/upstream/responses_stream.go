package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Responses 事件流翻译器（spec §9）
//
// 包一层 http.ResponseWriter：写入的字节是我们自己产出的 chat chunk SSE，
// 逐帧翻译成 Responses 事件序列。三个既有流式写入点一行不用改，DSML 还原与
// 思维链处理天然复用，不可能与 chat 路径漂移。
// ---------------------------------------------------------------------------

// NewResponsesWriter 包装 w，把写入的 chat chunk SSE 字节翻译成 Responses 事件序列。
// hintFn 非 nil 时，error 帧产出的 response.failed 会带 error.gateway_hint。
func NewResponsesWriter(w http.ResponseWriter, hintFn func(string) string) *ResponsesWriter {
	return &ResponsesWriter{
		ResponseWriter: w,
		hintFn:         hintFn,
		seq:            -1, // 首个事件为 0
		tools:          map[int]*responsesItem{},
	}
}

type responsesItem struct {
	kind      string // message / function_call / reasoning
	out       int    // output_index：创建时分配，全流唯一且与 output 数组下标一致
	id        string
	callID    string
	name      string
	text      string
	arguments string
	open      bool // 事件生命周期是否已开
	summary   string
}

// ResponsesWriter 实现 http.ResponseWriter 并显式提供 Flush。
type ResponsesWriter struct {
	http.ResponseWriter
	hintFn func(string) string

	mu sync.Mutex

	respID   string
	seq      int
	started  bool
	finished bool

	text     *responsesItem // message item
	reason   *responsesItem // reasoning item
	tools    map[int]*responsesItem
	toolKeys []int
	usage    map[string]any
	lastErr  string

	pending []byte // Write 边界之外的分帧缓冲
	out     []byte // 待写出的事件字节
	order   []*responsesItem // item 创建顺序，决定 output_index 与关闭顺序
}

// ObserveErrorFrame 记录上游 error 帧原文（供 WithErrorFrameObserver 使用，
// 让账号处置与 chat 路径同口径）。
func (rw *ResponsesWriter) ObserveErrorFrame(payload string) {
	rw.mu.Lock()
	rw.lastErr = payload
	rw.mu.Unlock()
}

// LastErrorFrame 返回最近的 error 帧原文（等价于 chat 路径的 errFrame 变量）。
func (rw *ResponsesWriter) LastErrorFrame() string {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.lastErr
}

// Flush 必须转发：三个流式写入点都是 fl, _ := w.(http.Flusher) 后逐帧 flush，
// 断言失败被 _ 吞掉，会让客户端在整个响应结束前收不到任何字节。
func (rw *ResponsesWriter) Flush() {
	rw.mu.Lock()
	rw.emitTo(rw.ResponseWriter)
	rw.mu.Unlock()
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *ResponsesWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *ResponsesWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	// 只缓冲 SSE 分帧需要的量级；不缓存「输出」以外的内容（spec §12）。
	rw.pending = append(rw.pending, p...)
	n := len(rw.pending)
	for {
		idx := bytes.Index(rw.pending, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := rw.pending[:idx]
		rw.pending = rw.pending[idx+2:]
		rw.handleFrame(frame)
	}
// 极端情况下（帧大于 4 MiB）不至于无限增长：截断并丢弃残帧。
	if len(rw.pending) > 4<<20 {
		rw.pending = rw.pending[:0]
	}
	rw.emitTo(rw.ResponseWriter)
	return n, nil
}

func (rw *ResponsesWriter) emitTo(w io.Writer) {
	if len(rw.out) == 0 {
		return
	}
	_, _ = w.Write(rw.out)
	rw.out = rw.out[:0]
}

func (rw *ResponsesWriter) handleFrame(frame []byte) {
	line := strings.TrimRight(string(frame), "\r\n \t")
	if !strings.HasPrefix(line, "data: ") {
		return // 只认 data: 前缀行；其余原样丢弃（我们自己产出的格式）
	}
	payload := strings.TrimSpace(line[len("data: "):])
	if payload == "[DONE]" {
		rw.finish()
		return
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		log.Printf("WARN: [upstream] responses stream: bad frame: %v", err)
		return
	}
	if e, ok := obj["error"]; ok && e != nil {
		rw.fail(payload, e)
		return
	}
	rw.consumeChunk(obj)
}

// consumeChunk 把一帧 chat chunk 翻译成事件。
func (rw *ResponsesWriter) consumeChunk(obj map[string]any) {
	rw.ensureStarted(obj)
	if u, ok := obj["usage"].(map[string]any); ok {
		rw.usage = map[string]any{}
		renameTok(u, rw.usage, "prompt_tokens", "input_tokens")
		renameTok(u, rw.usage, "completion_tokens", "output_tokens")
		renameTok(u, rw.usage, "total_tokens", "total_tokens")
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)

	// 正文：切到工具调用时先把文本 item 收尾。
	if content, ok := delta["content"].(string); ok && content != "" {
		if rw.text == nil {
			rw.text = rw.addItem(&responsesItem{kind: "message", id: "msg_" + rw.respSuffix()})
			rw.emit("response.output_item.added", map[string]any{
				"output_index": rw.text.out,
				"item":         rw.text.view(),
			})
			rw.emit("response.content_part.added", map[string]any{
				"item_id":   rw.text.id,
				"output_index": rw.text.out,
				"content_index": 0,
				"part":     map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
			rw.text.open = true
		}
		rw.text.text += content
		rw.emit("response.output_text.delta", map[string]any{
			"item_id":       rw.text.id,
			"output_index":  rw.text.out,
			"content_index": 0,
			"delta":         content,
		})
	}

	// 思维链：映射为 reasoning item 的 summary 增量；绝不产 encrypted_content。
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		rw.ensureReasoning()
		rw.reason.summary += rc
		rw.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id":      rw.reason.id,
			"output_index": rw.reason.out,
			"summary_index": 0,
			"delta":        rc,
		})
	}

	// 工具调用：按 index 开/推进。
	for _, raw := range sliceAny(delta["tool_calls"]) {
		tc, _ := raw.(map[string]any)
		idx := intOf(tc["index"])
		item := rw.tools[idx]
		if item == nil {
			rw.closeOpenItems()
			item = rw.addItem(&responsesItem{kind: "function_call", callID: str(tc["id"])})
			if item.callID == "" {
				item.callID = "call_" + strconv.Itoa(idx) + "_" + rw.respSuffix()
			}
			item.id = "fc_" + item.callID
			rw.tools[idx] = item
			rw.toolKeys = append(rw.toolKeys, idx)
			fn, _ := tc["function"].(map[string]any)
			item.name = str(fn["name"])
			item.open = true
			rw.emit("response.output_item.added", map[string]any{
				"output_index": item.out,
				"item":         item.view(),
			})
		}
		if n := str(fn2(tc)["name"]); n != "" {
			item.name = n
		}
		if a := str(fn2(tc)["arguments"]); a != "" {
			item.arguments += a
			rw.emit("response.function_call_arguments.delta", map[string]any{
				"item_id":      item.id,
				"output_index": item.out,
				"delta":        a,
			})
		}
	}

	if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
		rw.closeOpenItems()
	}
}

func fn2(tc map[string]any) map[string]any {
	fn, _ := tc["function"].(map[string]any)
	return fn
}

func (rw *ResponsesWriter) ensureStarted(obj map[string]any) {
	if rw.started {
		return
	}
	rw.started = true
	if id := str(obj["id"]); id != "" {
		rw.respID = "resp_" + id
	} else {
		rw.respID = "resp_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	rw.emit("response.created", map[string]any{
		"response": map[string]any{"id": rw.respID, "object": "response", "status": "in_progress"},
	})
	rw.emit("response.in_progress", map[string]any{
		"response": map[string]any{"id": rw.respID, "object": "response", "status": "in_progress"},
	})
}

func (rw *ResponsesWriter) ensureReasoning() {
	if rw.reason != nil {
		return
	}
	rw.reason = rw.addItem(&responsesItem{kind: "reasoning", id: "rs_" + rw.respSuffix(), open: true})
	rw.emit("response.output_item.added", map[string]any{
		"output_index": rw.reason.out,
		"item": map[string]any{
			"type": "reasoning", "id": rw.reason.id, "summary": []any{},
		},
	})
	rw.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id":      rw.reason.id,
		"output_index": rw.reason.out,
		"summary_index": 0,
		"part":         map[string]any{"type": "summary_text", "text": ""},
	})
}

func (rw *ResponsesWriter) closeText() {
	if rw.text == nil || !rw.text.open {
		return
	}
	rw.text.open = false
	rw.emit("response.output_text.done", map[string]any{
		"item_id": rw.text.id, "output_index": rw.text.out, "content_index": 0,
		"text": rw.text.text,
	})
	rw.emit("response.content_part.done", map[string]any{
		"item_id": rw.text.id, "output_index": rw.text.out, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": rw.text.text, "annotations": []any{}},
	})
	rw.emit("response.output_item.done", map[string]any{
		"output_index": rw.text.out, "item": rw.text.view(),
	})
}

func (rw *ResponsesWriter) closeReasoning() {
	if rw.reason == nil || !rw.reason.open {
		return
	}
	rw.reason.open = false
	rw.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": rw.reason.id, "output_index": rw.reason.out, "summary_index": 0,
		"text": rw.reason.summary,
	})
	rw.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": rw.reason.id, "output_index": rw.reason.out, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": rw.reason.summary},
	})
	rw.emit("response.output_item.done", map[string]any{
		"output_index": rw.reason.out,
		"item": map[string]any{
			"type": "reasoning", "id": rw.reason.id,
			"summary": []any{map[string]any{"type": "summary_text", "text": rw.reason.summary}},
		},
	})
}

func (rw *ResponsesWriter) closeAllTools() {
	for _, idx := range rw.toolKeys {
		item := rw.tools[idx]
		if item == nil || !item.open {
			continue
		}
		item.open = false
		rw.emit("response.function_call_arguments.done", map[string]any{
			"item_id": item.id, "output_index": item.out, "arguments": item.arguments,
		})
		rw.emit("response.output_item.done", map[string]any{
			"output_index": item.out, "item": item.view(),
		})
	}
}

// addItem 登记一个新 item 并分配全流唯一的 output_index（= 创建顺序）。
func (rw *ResponsesWriter) addItem(it *responsesItem) *responsesItem {
	it.out = len(rw.order)
	rw.order = append(rw.order, it)
	return it
}

// closeOpenItems 按 output_index 升序收尾所有未关闭的 item——客户端按
// output_index 驱动 item 生命周期，乱序 done 会让 UI 提前把后续 item 判成孤儿。
func (rw *ResponsesWriter) closeOpenItems() {
	for _, it := range rw.order {
		if !it.open {
			continue
		}
		it.open = false
		switch it.kind {
		case "message":
			rw.emit("response.output_text.done", map[string]any{
				"item_id": it.id, "output_index": it.out, "content_index": 0, "text": it.text,
			})
			rw.emit("response.content_part.done", map[string]any{
				"item_id": it.id, "output_index": it.out, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": it.text, "annotations": []any{}},
			})
		case "reasoning":
			rw.emit("response.reasoning_summary_text.done", map[string]any{
				"item_id": it.id, "output_index": it.out, "summary_index": 0, "text": it.summary,
			})
			rw.emit("response.reasoning_summary_part.done", map[string]any{
				"item_id": it.id, "output_index": it.out, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": it.summary},
			})
		case "function_call":
			rw.emit("response.function_call_arguments.done", map[string]any{
				"item_id": it.id, "output_index": it.out, "arguments": it.arguments,
			})
		}
		rw.emit("response.output_item.done", map[string]any{"output_index": it.out, "item": it.view()})
	}
}

func (rw *ResponsesWriter) toolOpen() bool {
	for _, idx := range rw.toolKeys {
		if it := rw.tools[idx]; it != nil && it.open {
			return true
		}
	}
	return false
}

func (rw *ResponsesWriter) finish() {
	if rw.finished {
		return
	}
	if !rw.started {
		// 空流兜底：仍产一个空的 completed，客户端不会无限等。
		rw.ensureStarted(nil)
	}
	rw.finished = true
	rw.closeOpenItems()
	resp := map[string]any{
		"id": rw.respID, "object": "response", "status": "completed",
		"output": rw.outputItems(),
	}
	if rw.usage != nil {
		resp["usage"] = rw.usage
	}
	rw.emit("response.completed", map[string]any{"response": resp})
}

func (rw *ResponsesWriter) fail(payload string, e any) {
	if rw.finished {
		return
	}
	rw.lastErr = payload // 已在 Write 内持锁，不能再走 ObserveErrorFrame
	if !rw.started {
		rw.ensureStarted(nil)
	}
	rw.finished = true
	rw.closeOpenItems()
	errmsg, _ := e.(map[string]any)
	errObj := map[string]any{
		"type":    "api_error",
		"message": str(errmsg["message"]),
	}
	if c, ok := errmsg["code"]; ok {
		errObj["code"] = c
	}
	if rw.hintFn != nil {
		if h := rw.hintFn(str(errmsg["message"])); h != "" {
			errObj["gateway_hint"] = h
		}
	}
	rw.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id": rw.respID, "object": "response", "status": "failed",
			"error": errObj,
		},
	})
	// 关键：error 帧之后不得再发 response.completed（Review Focus #4）。
}

// outputItems 返回已产出的完整 item 数组（response.completed 必须带完整 output）。
// 按创建顺序（即 output_index 升序）返回，与事件里的 output_index 严格对应。
func (rw *ResponsesWriter) outputItems() []any {
	items := make([]any, 0, len(rw.order))
	for _, it := range rw.order {
		items = append(items, it.view())
	}
	return items
}

func (i *responsesItem) view() map[string]any {
	switch i.kind {
	case "message":
		return map[string]any{
			"type": "message", "id": i.id, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": i.text, "annotations": []any{},
			}},
		}
	case "function_call":
		return map[string]any{
			"type": "function_call", "id": i.id, "call_id": i.callID,
			"name": i.name, "arguments": i.arguments, "status": "completed",
		}
	default:
		return map[string]any{
			"type": "reasoning", "id": i.id,
			"summary": []any{map[string]any{"type": "summary_text", "text": i.summary}},
		}
	}
}

func (rw *ResponsesWriter) respSuffix() string { return strings.TrimPrefix(rw.respID, "resp_") }

// emit 序列出一条事件并追加到待写缓冲。
func (rw *ResponsesWriter) emit(typ string, extra map[string]any) {
	rw.seq++
	body := map[string]any{
		"type":            typ,
		"sequence_number": rw.seq,
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	rw.out = append(rw.out, []byte("event: "+typ+"\ndata: "+string(raw)+"\n\n")...)
}

func renameTok(src, dst map[string]any, from, to string) {
	if v, ok := src[from]; ok {
		dst[to] = v
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func sliceAny(v any) []any {
	s, _ := v.([]any)
	return s
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

