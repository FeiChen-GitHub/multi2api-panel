package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// DESIGN §4.4 / §15.3 决议 6：Trae phase 1 只支持 cn，`trae:global:*` 必须返回
// 明确的 400 realm_not_supported，而不是让它走到选号层报成 503 no_healthy_account
//（那会被读成「号全挂了」，而非「这个 realm 不支持」）。
func TestChatTraeGlobalRealmRejected(t *testing.T) {
	wbCalls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		wbCalls++
		return 200, sseOK, true
	})
	traeCalls := 0
	tc := traeTestClient(t, &traeCalls)

	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	body := `{"model":"trae:global:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if resp.Error.Code != "realm_not_supported" {
		t.Fatalf("error.code = %q, want realm_not_supported", resp.Error.Code)
	}
	if traeCalls != 0 || wbCalls != 0 {
		t.Fatalf("拒绝的请求不得触达任何上游：trae=%d wb=%d", traeCalls, wbCalls)
	}
}

// trae:cn:* 不受该闸影响（零回归）。
func TestChatTraeCNRealmStillServed(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	traeCalls := 0
	tc := traeTestClient(t, &traeCalls)
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := testPoolWith(traeAuth)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	body := `{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if traeCalls != 1 {
		t.Fatalf("trae upstream calls = %d, want 1", traeCalls)
	}
}
