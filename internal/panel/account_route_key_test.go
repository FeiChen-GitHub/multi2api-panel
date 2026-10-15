package panel

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 面板账号路由参数必须是 AccountKey（DESIGN §9.2 D5 / §11），不是 uid：
// uid 只在单个 provider 内唯一，跨 provider 撞号时会操作到另一个账号。
func TestAccountRoutesUseAccountKey(t *testing.T) {
	const key = "33333333-3333-4333-8333-333333333333"
	a, err := auth.Parse([]byte(`{"account_key":"` + key + `","provider":"trae",` +
		`"auth":{"accessToken":"at","expiresAt":9999999999},"account":{"uid":"same-uid"}}`))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	if a.Key() != auth.AccountKey(key) {
		t.Fatalf("解析出的主键 = %q, want %q", a.Key(), key)
	}
	p := New(Config{Version: "test", APIKey: "k", AuthDir: t.TempDir(), Pool: pool.New(""), Upstream: &upstream.Client{}})
	p.cfg.Pool.Add(a)

	// 用主键：命中，暂停成功。
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, authReq("POST", "/panel/api/accounts/"+key+"/pause"))
	if rr.Code != 200 {
		t.Fatalf("按 key 调用 code=%d body=%s, want 200", rr.Code, rr.Body.String())
	}

	// 用 uid：必须 404（uid 不再是合法路由参数，否则跨 provider 撞号会误伤）。
	rr2 := httptest.NewRecorder()
	p.ServeHTTP(rr2, authReq("POST", "/panel/api/accounts/same-uid/pause"))
	if rr2.Code != 404 {
		t.Fatalf("按 uid 调用 code=%d body=%s, want 404", rr2.Code, rr2.Body.String())
	}

	// 不存在的 key 同样 404。
	rr3 := httptest.NewRecorder()
	p.ServeHTTP(rr3, authReq("POST", "/panel/api/accounts/44444444-4444-4444-8444-444444444444/revive"))
	if rr3.Code != 404 {
		t.Fatalf("未知 key code=%d, want 404", rr3.Code)
	}
}

// 任务类路由同样按主键解析（Trae 账号在 tasks 路由上应报 provider 不支持，
// 而不是「账号不存在」——说明是 key 解析成功后才做的 provider 判定）。
func TestTaskRoutesResolveByKey(t *testing.T) {
	const key = "55555555-5555-4555-8555-555555555555"
	a, err := auth.Parse([]byte(`{"account_key":"` + key + `","provider":"trae",` +
		`"auth":{"accessToken":"at","expiresAt":9999999999},"account":{"uid":"t-1"}}`))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	p := New(Config{Version: "test", APIKey: "k", AuthDir: t.TempDir(), Pool: pool.New(""), Upstream: &upstream.Client{}})
	p.cfg.Pool.Add(a)

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, authReq("GET", "/panel/api/accounts/"+key+"/tasks"))
	if rr.Code != 400 {
		t.Fatalf("code=%d body=%s, want 400（Trae 无成长任务体系）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "无成长任务体系") {
		t.Fatalf("body=%s, want provider 提示（说明 key 已解析到账号）", rr.Body.String())
	}
}
