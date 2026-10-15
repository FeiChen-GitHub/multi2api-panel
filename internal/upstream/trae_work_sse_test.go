package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Work 的 output.text 是**累积全文**，不是增量。直接透传会让客户端收到
// 「he」「hello」「hello world」这种逐帧重复全文——必须差分后再下发。
func TestWorkStreamDiffsCumulativeText(t *testing.T) {
	stream := "" +
		"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"he\"}]}}\n\n" +
		"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"hello\"}]}}\n\n" +
		"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"hello world\"}]}}\n\n" +
		"data: {\"event\":\"token_usage\",\"payload\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n" +
		"data: {\"event\":\"done\",\"payload\":{}}\n\n"

	rec := httptest.NewRecorder()
	if err := TraeStreamWorkToOpenAI(rec, strings.NewReader(stream), "DeepSeek-V4-Flash-Official", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()

	var content strings.Builder
	var doneCount, usageSeen int
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			doneCount++
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if len(chunk.Choices) == 1 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
		if chunk.Usage != nil {
			usageSeen++
		}
	}
	if got := content.String(); got != "hello world" {
		t.Fatalf("content = %q, want %q（累积全文必须差分，否则逐帧重复）", got, "hello world")
	}
	if doneCount != 1 {
		t.Fatalf("[DONE] count = %d, want 1", doneCount)
	}
	if usageSeen != 1 {
		t.Fatalf("usage chunks = %d, want 1", usageSeen)
	}
}

// plan_item 走思考通道（reasoning_content），done 的尾包正文要补进 content。
func TestWorkStreamSplitsReasoningAndContent(t *testing.T) {
	stream := "" +
		"data: {\"event\":\"plan_item\",\"payload\":{\"thought\":\"let me think\"}}\n\n" +
		"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"answer\"}]}}\n\n" +
		"data: {\"event\":\"done\",\"payload\":{\"last_assistant_response\":\"[\\\"answer!\\\"]\"}}\n\n"

	rec := httptest.NewRecorder()
	if err := TraeStreamWorkToOpenAI(rec, strings.NewReader(stream), "m", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"let me think"`) {
		t.Fatalf("thought should go to reasoning_content, body=%s", body)
	}
	if !strings.Contains(body, `"content":"answer"`) {
		t.Fatalf("output text should go to content, body=%s", body)
	}
	// done.last_assistant_response 是「累积全文」（"answer!"），与已下发的 "answer"
	// 差分后只剩 "!" —— 直接透传会把全文再发一遍。
	if !strings.Contains(body, `"content":"!"`) {
		t.Fatalf("done.last_assistant_response should be diffed to tail delta \"!\", body=%s", body)
	}
	if strings.Count(body, `"content":"answer!"`) != 0 {
		t.Fatalf("done full text must not be re-emitted verbatim, body=%s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("want exactly one [DONE], body=%s", body)
	}
}

// 流内 error 事件：回调拿到错误码，且仍然保证写 [DONE]（客户端不会挂住）。
func TestWorkStreamErrorEvent(t *testing.T) {
	stream := "data: {\"event\":\"error\",\"code\":4001,\"message\":\"param invalid\"}\n\n"
	rec := httptest.NewRecorder()
	var got *TraeStreamError
	if err := TraeStreamWorkToOpenAI(rec, strings.NewReader(stream), "m", func(se *TraeStreamError) { got = se }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got == nil || got.Code != 4001 {
		t.Fatalf("onErr = %+v, want code 4001", got)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("error stream must still end with [DONE], body=%s", rec.Body.String())
	}
}

// 上游中断（无 done 帧）也要补 [DONE]。
func TestWorkStreamTruncatedStillDone(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := TraeStreamWorkToOpenAI(rec, strings.NewReader("data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"partial\"}]}}\n\n"), "m", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("truncated stream must end with exactly one [DONE], body=%s", rec.Body.String())
	}
}

// 已是标准 OpenAI chunk 的帧（桥接/上游直出）原样透传，不重新解析。
func TestWorkStreamPassthroughOpenAIChunk(t *testing.T) {
	in := "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	rec := httptest.NewRecorder()
	if err := TraeStreamWorkToOpenAI(rec, strings.NewReader(in), "m", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"chat.completion.chunk"`) || !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("OpenAI chunk should pass through untouched, body=%s", rec.Body.String())
	}
}

// 非流式聚合：累积全文差分后拼成完整正文，usage 与 error 各归其位。
func TestAggregateWork(t *testing.T) {
	stream := "" +
		"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"he\"}]}}\n\n" +
		"data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"hello\"}]}}\n\n" +
		"data: {\"event\":\"token_usage\",\"payload\":{\"total_tokens\":2}}\n\n" +
		"data: {\"event\":\"done\",\"payload\":{}}\n\n"
	resp, err := TraeAggregateWork(strings.NewReader(stream), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"content":"hello"`) {
		t.Fatalf("aggregate content wrong: %s", raw)
	}
	if !strings.Contains(string(raw), `"total_tokens":2`) {
		t.Fatalf("aggregate usage missing: %s", raw)
	}

	// 流内 error → 返回 *TraeStreamError
	_, err = TraeAggregateWork(strings.NewReader("data: {\"event\":\"error\",\"code\":4008,\"message\":\"quota\"}\n\n"), "m")
	se, ok := err.(*TraeStreamError)
	if !ok || se.Code != 4008 {
		t.Fatalf("aggregate err = %v (%T), want *TraeStreamError code 4008", err, err)
	}
}

// 空数组占位 "[]" 不是内容，不得被当成正文下发。
func TestWorkStreamIgnoresEmptyArrayPlaceholder(t *testing.T) {
	rec := httptest.NewRecorder()
	in := "data: {\"event\":\"output\",\"payload\":{\"choices\":[{\"text\":\"[]\"}]}}\n\ndata: {\"event\":\"done\",\"payload\":{}}\n\n"
	if err := TraeStreamWorkToOpenAI(rec, strings.NewReader(in), "m", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Contains(rec.Body.String(), `"content":"[]"`) {
		t.Fatalf("\"[]\" placeholder must not be emitted, body=%s", rec.Body.String())
	}
}
