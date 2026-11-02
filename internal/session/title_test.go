package session

import "testing"

func TestConversationTitle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"first user text", `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"帮我分析这个仓库"},{"role":"assistant","content":"好"}]}`, "帮我分析这个仓库"},
		{"multimodal joins text", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "看图"},
		{"no user", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"broken", `{broken`, ""},
		{"env context skipped", `{"messages":[{"role":"user","content":"<environment_context> <cwd>C:/x</cwd>"},{"role":"user","content":"真实问题"}]}`, "真实问题"},
		{"env context only", `{"messages":[{"role":"user","content":"<environment_context> <cwd>C:/x</cwd>"}]}`, ""},
		{"agents instructions skipped", `{"messages":[{"role":"user","content":"# AGENTS.md instructions for C:/data/github/multi2api-panel <INSTRUCTIONS> # AGENTS.md ..."},{"role":"user","content":"帮我改个 bug"}]}`, "帮我改个 bug"},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"long truncated", `{"messages":[{"role":"user","content":"` + longStr(200) + `"}]}`, longStr(120)},
		{"responses input user text", `{"input":[{"role":"user","content":[{"type":"input_text","text":"responses 标题"}]}]}`, "responses 标题"},
		{"responses input message string", `{"input":[{"role":"user","content":"纯字符串"}]}`, "纯字符串"},
		{"responses input non-message first", `{"input":[{"type":"function_call","name":"f"},{"role":"user","content":"第二项"}]}`, "第二项"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ConversationTitle([]byte(c.body)); got != c.want {
				t.Errorf("ConversationTitle = %q want %q", got, c.want)
			}
		})
	}
}

func longStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}


