package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// 带会话键的 chat 请求应把最后一条 user 消息写入进程内缓存（不落盘）。
func TestChatWritesUserMessageCache(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	cache := session.NewUserMessageCache(10, 0)
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     up,
		UserMessages: cache,
	})
	body := `{"model":"wb:glm-5.2","metadata":{"conversation_id":"conv-1"},"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"最后这个问题"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 粘性键 = provider|sessKey；provider 由模型前缀决定（wb）
	key := "workbuddy|conv-1"
	got, ok := cache.Get(key)
	if !ok {
		t.Fatalf("cache miss for %q", key)
	}
	if got != "最后这个问题" {
		t.Fatalf("cached = %q want 最后这个问题", got)
	}
}
