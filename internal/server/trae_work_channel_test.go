package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// traeWorkTestPool 构造一个单 Trae 账号的池。
func traeWorkTestPool(t *testing.T) (*pool.Pool, *auth.Auth) {
	t.Helper()
	a, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	return testPoolWith(a), a
}

// Work 通道撞 work_credits 不足（400 + credit 文案）→ 只冷 Work 通道，
// SOLO 出口必须仍然可用（DESIGN §6.6「一账号两出口」+ §15.5 D8）。
// 没有通道维度时，这一次失败会把整个账号冷到次日 04:00，SOLO 一起陪葬。
func TestTraeWorkCreditErrorOnlyCoolsWorkChannel(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == upstream.TraeEpCreateAgentTask {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"message":"insufficient work credit"}`)),
			}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(traeSOLOFixture))}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	p, a := traeWorkTestPool(t)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"hi"}]}`)))

	st, ok := p.Status(a.UID)
	if !ok {
		t.Fatal("account missing from pool")
	}
	if !st.WorkCooling {
		t.Fatalf("Work 通道应被冷却（work_cooling=false）: %+v", st)
	}
	if st.Cooling {
		t.Fatalf("账号级冷却不应被 Work 失败触发（SOLO 出口被连坐）: %+v", st)
	}
	// SOLO 出口仍可路由到这个账号。
	if got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, pool.ChannelSOLO); got == nil {
		t.Fatal("SOLO 通道应仍能选中该账号")
	}
	if got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, pool.ChannelWork); got != nil {
		t.Fatalf("Work 通道冷却后不应再选中该账号: %+v", got)
	}
}

// Work 通道的原生任务错误（4001）不该被当成额度耗尽 —— 那是参数/配置问题，
// 冷却额度毫无意义。钉住它不会落到 Work 冷却上。
func TestTraeWorkNativeErrorDoesNotCoolWorkChannel(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":4001,"message":"failed to get summary config"}`)),
		}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	p, a := traeWorkTestPool(t)
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:DeepSeek-V4-Flash-Official","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status(a.UID)
	if st.WorkCooling {
		t.Fatalf("4001 是参数错误，不该把 Work 通道按额度耗尽冷却: %+v", st)
	}
}

// 反过来：SOLO 撞权益不足仍然冷账号级（SOLO 就是它触发的），且 Work 出口不受影响。
func TestTraeSoloCreditErrorCoolsAccountOnly(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	tc := upstream.NewTraeClient()
	tc.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 400,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":1005,"message":"plan limit"}`)),
		}, nil
	})}
	tc.StreamHTTP = tc.HTTP

	p, a := traeWorkTestPool(t)
	// 关掉 Work 降级，隔离「SOLO 失败 → 账号级冷却」这一条路径。
	h := NewHandler(Config{Pool: p, Upstream: up, Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc), TraeWorkDisabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"trae:cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status(a.UID)
	if !st.Cooling {
		t.Fatalf("SOLO 权益不足应触发账号级硬冷却: %+v", st)
	}
	if st.WorkCooling {
		t.Fatalf("SOLO 失败不该写 Work 通道冷却: %+v", st)
	}
}
