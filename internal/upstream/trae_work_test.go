package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 提取最后一条 user 输入：字符串与多模态数组两种形态都要能取到。
func TestTraeExtractLastUserPrompt(t *testing.T) {
	cases := []struct {
		name string
		msgs []any
		want string
	}{
		{
			name: "string content",
			msgs: []any{
				map[string]any{"role": "user", "content": "first"},
				map[string]any{"role": "assistant", "content": "mid"},
				map[string]any{"role": "user", "content": "last"},
			},
			want: "last",
		},
		{
			name: "array content",
			msgs: []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "hello"},
					map[string]any{"type": "image_url"},
					map[string]any{"type": "text", "text": "world"},
				}},
			},
			want: "helloworld",
		},
		{
			name: "no user message",
			msgs: []any{map[string]any{"role": "assistant", "content": "x"}},
			want: "你好",
		},
	}
	for _, tc := range cases {
		if got := TraeExtractLastUserPrompt(tc.msgs); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// Work 通道走 create_agent_task：不同 host、不同端点、不同 payload 形态。
func TestTraeChatStreamWorkChannelUsesCreateAgentTask(t *testing.T) {
	var gotHost, gotPath, gotBody, gotAppID string
	c := NewTraeClient()
	c.HTTP = &http.Client{Transport: traeRTFunc(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Host
		gotPath = r.URL.Path
		gotAppID = r.Header.Get("X-App-Id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{}\n\n")),
		}, nil
	})}
	c.StreamHTTP = c.HTTP

	a := &auth.Auth{AccessToken: "jwt", UID: "u1", DeviceID: "1234567890123456"}
	body := `{"model":"DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"写个函数"}]}`
	rc, status, respBody, err := c.ChatStream(context.Background(), a, []byte(body), "", ChatMeta{}, ChannelWork)
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v body=%s", status, err, respBody)
	}
	defer rc.Close()

	if gotHost != "api5-normal.mchost.guru" {
		t.Fatalf("host = %q, want api5-normal.mchost.guru", gotHost)
	}
	if gotPath != TraeEpCreateAgentTask {
		t.Fatalf("path = %q, want %q", gotPath, TraeEpCreateAgentTask)
	}
	if gotAppID != TraeWorkAppIDChat {
		t.Fatalf("X-App-Id = %q, want %q", gotAppID, TraeWorkAppIDChat)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("work body not JSON: %v (%s)", err, gotBody)
	}
	if sent["agent_type"] != TraeFunction {
		t.Fatalf("agent_type = %v, want %q", sent["agent_type"], TraeFunction)
	}
	if sent["config_name"] != "DeepSeek-V4-Flash-Official" {
		t.Fatalf("config_name = %v", sent["config_name"])
	}
	if mn, _ := sent["model_name"].(string); !strings.HasSuffix(mn, "__dev") {
		t.Fatalf("model_name = %q, want __dev suffix", mn)
	}
	ui, ok := sent["user_input"].(map[string]any)
	if !ok {
		t.Fatalf("user_input = %#v", sent["user_input"])
	}
	msgs, ok := ui["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("user_input.messages = %#v", ui["messages"])
	}
	if msgs[0].(map[string]any)["content"] != "写个函数" {
		t.Fatalf("message content = %v", msgs[0].(map[string]any)["content"])
	}
}

// SOLO 通道仍走 llm_utils_chat（与 Work 分支互不影响）。
func TestTraeChatStreamSOLOStaysOnAgentHost(t *testing.T) {
	var gotHost, gotPath string
	c := NewTraeClient()
	c.HTTP = &http.Client{Transport: traeRTFunc(func(r *http.Request) (*http.Response, error) {
		gotHost, gotPath = r.URL.Host, r.URL.Path
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	c.StreamHTTP = c.HTTP

	a := &auth.Auth{AccessToken: "jwt", UID: "u1"}
	rc, _, _, err := c.ChatStream(context.Background(), a, []byte(`{"model":"glm-5.2"}`), "", ChatMeta{}, ChannelSOLO)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if gotHost != "trae-api-cn.mchost.guru" || gotPath != TraeEpChat {
		t.Fatalf("solo host/path = %q/%q", gotHost, gotPath)
	}
}

type traeRTFunc func(*http.Request) (*http.Response, error)

func (f traeRTFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
