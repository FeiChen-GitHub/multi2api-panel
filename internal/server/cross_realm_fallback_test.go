// cross_realm_fallback_test.go 同 provider 内跨域回退（issue #135）端到端测试。
//
// 本仓模型名协议要求 provider 必填（DESIGN §3.1），因此回退的触发形态是
// 「写了 provider、没写 realm」（如 wb:glm-5.2），而不是 main 分支的裸模型名。
// 语义边界：
//   - 开关默认关闭 = 零回归（行为与引入前完全一致）；
//   - 开启后首选域选不到号 / 首选域 11102 → 改用同 provider 的另一域；
//   - 显式 wb:cn: / wb:global: 永不回退（那是用户的明确意图）；
//   - 绝不跨 provider 借号（模型名、计费、上游协议都不同）。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newRealmPool 构造 cn + global 各一个 WorkBuddy 账号的池子。
// global 号按 domain 后缀归一化（www.workbuddy.ai → global）；cn 号积分更高，
// 配合 testPoolWith 的确定性随机源，断言「先试 cn、失败后才回退 global」。
func newRealmPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "gl1", AccessToken: "at-gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	p.SetCredits("cn1", 2000, 0)
	p.SetCredits("gl1", 1000, 0)
	return p
}

// 11102「该后端无此模型」是 issue #135 的核心场景：模型只存在于 global 目录，
// 未写 realm 的请求先被送到 cn 号 → 11102；开启回退后应改用 global 号拿到 200。
func TestCrossRealmFallbackOnModelBlocked(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-cn" {
			return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
		}
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: 未写 realm 的请求在 cn 11102 后必须回退 global)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want cn 1 次（11102）→ global 1 次（回退成功）", calls)
	}
}

// 开关缺省关闭：行为与引入前完全一致——11102 后不跨域，回 400 model_unavailable。
func TestCrossRealmFallbackDisabledByDefault(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
	})
	p := newRealmPool(t) // 不调用 SetCrossDomainFallback
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: 开关关闭时不跨域)", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 0 {
		t.Errorf("calls=%v want global 0 次（开关关闭不得跨域）", calls)
	}
}

// 显式 realm 前缀永不回退：wb:cn:xxx 即使在 cn 撞 11102 也不许改用 global。
func TestCrossRealmFallbackNeverOverridesExplicitRealm(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:cn:deepseek-v4.1-flash-sg","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: 显式 cn: 前缀不得回退)", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 0 {
		t.Errorf("calls=%v want global 0 次（显式 realm 永不回退）", calls)
	}
}

// 首选域没有可服务号时直接换域：cn 号全部冷却，未写 realm 的请求应立刻落到 global，
// 不必先拿冷却号白撞一次上游。
func TestCrossRealmFallbackWhenPreferredRealmHasNoServableAccount(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 200, sseOK, true
	})
	p := newRealmPool(t)
	p.SetCrossDomainFallback(true)
	// 把 cn 号软冷却一小时：healthyForChannel 会判它不可服务。
	p.Cooldown("cn1", pool.CoolSoft, time.Hour, "test soft rate")

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: cn 无可用号时直接换 global)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 || calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want cn 0 次（直接换域）→ global 1 次", calls)
	}
}

// 绝不跨 provider：Trae 只有 cn，wb 的 cn 号冷却后不得拿 Trae 号顶替。
// 这里用「只有 Trae 号、请求要 wb」的最简形态验证 provider 过滤在回退路径上依然生效。
func TestCrossRealmFallbackNeverCrossesProvider(t *testing.T) {
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
	p := testPoolWith(traeAuth) // 池里只有 Trae 号，没有 WorkBuddy 号
	p.SetCrossDomainFallback(true)
	h := NewHandler(Config{Pool: p, Upstream: up,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"wb:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503: 池里没有 WorkBuddy 号)", rec.Code, rec.Body)
	}
	if wbCalls != 0 || traeCalls != 0 {
		t.Fatalf("不得跨 provider 借号：wb=%d trae=%d", wbCalls, traeCalls)
	}
}
