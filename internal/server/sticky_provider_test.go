package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 粘性绑定必须按 provider 隔离。
//
// 绑定只记账号主键、**不区分 provider**，而 Trae 与 WorkBuddy 的 realm 同为 cn ——
// 选号处那条 realm 判据拦不住跨 provider 命中。于是同一会话从 wb:* 切到 trae:* 时，
// 粘性会把上一次的 WorkBuddy 号交出来，拿它的 token 去打 Trae 端点；上游回 401，
// TraeClassify 判成 ErrSessionDead，applyErrorPolicy 随即**把那个 WorkBuddy 账号
// 永久禁用**（reason "12153 session dead"，需人工重登）。用户侧观感就是「Trae 一
// 失败，WorkBuddy 跟着废掉」。
//
// 修法：粘性键带 provider 维度（stickySessKey = provider|sessKey），两个 provider
// 各自维持会话亲和性；外加一条 provider 校验做纵深防御。
func TestChatStickyIsolatedPerProvider(t *testing.T) {
	wbAuth := &auth.Auth{UID: "wb1", AccessToken: "at-wb", ExpiresAt: 9999999999}
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(wbAuth, traeAuth)
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     newBindStore(),
		Available: func() []string { return []string{string(wbAuth.Key()), string(traeAuth.Key())} },
	})

	wbCalls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		wbCalls++
		return 200, sseOK, true
	})

	traeCalls := 0
	var traeAuthz string
	tc := upstream.NewTraeClient()
	tc.AgentHost = "https://trae.fake"
	tc.UgHost = "https://trae.fake"
	tc.OAuthHost = "https://trae.fake"
	fake := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		traeCalls++
		traeAuthz = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(traeSOLOFixture)),
		}, nil
	})}
	tc.HTTP = fake
	tc.StreamHTTP = fake

	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		Dispatcher:   upstream.NewDispatcher(upstream.NewWBClient(up), tc),
		SoftCooldown: time.Minute,
	})

	// 第一轮：wb 模型成功 → 会话绑定到 WorkBuddy 号。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:cn:glm-5.2","conversation_id":"c1","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("wb code=%d body=%s", rec.Code, rec.Body.String())
	}
	if wbCalls != 1 {
		t.Fatalf("wb upstream calls = %d, want 1", wbCalls)
	}

	// 第二轮：同一会话键切到 trae 模型。粘性不得把 WorkBuddy 号交出来。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","conversation_id":"c1","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("trae code=%d body=%s", rec.Code, rec.Body.String())
	}
	if traeCalls != 1 {
		t.Fatalf("trae upstream calls = %d, want 1", traeCalls)
	}
	if want := "Cloud-IDE-JWT trae-at"; traeAuthz != want {
		t.Fatalf("trae 请求携带的凭证 = %q, want %q（粘性串号：拿 WorkBuddy 的 token 打了 Trae 端点）",
			traeAuthz, want)
	}
	if wbCalls != 1 {
		t.Fatalf("第二轮后 wb upstream calls = %d, want 1（Trae 请求不得打到 WorkBuddy 端点）", wbCalls)
	}
	if st, ok := p.Status(wbAuth.UID); ok && st.Disabled {
		t.Fatalf("WorkBuddy 账号被禁用（reason=%q）：Trae 失败不得连坐 WorkBuddy", st.DisabledReason)
	}
}
