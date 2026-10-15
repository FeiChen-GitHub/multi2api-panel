package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 登录 URL 必须带上回调落点与 machine/device 对，且 login_trace_id 可由该对派生
// （回调只回传 trace id，靠它反查 pending 找回这一对 id）。
func TestTraeLoginURLCarriesTraceAndCallback(t *testing.T) {
	machine := strings.Repeat("a", 32)
	device := "1234567890123456"
	u := TraeLoginURL(machine, device, "http://127.0.0.1:18080/authorize")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	q := parsed.Query()
	if got := q.Get("auth_callback_url"); got != "http://127.0.0.1:18080/authorize" {
		t.Fatalf("auth_callback_url = %q", got)
	}
	if got := q.Get("machine_id"); got != machine {
		t.Fatalf("machine_id = %q, want %q", got, machine)
	}
	if got := q.Get("device_id"); got != device {
		t.Fatalf("device_id = %q, want %q", got, device)
	}
	if got := q.Get("login_trace_id"); got != traeTraceID(machine, device) {
		t.Fatalf("login_trace_id = %q, want %q（必须由 machine+device 派生，否则回调反查不到）", got, traeTraceID(machine, device))
	}
}

// 回调解析：refreshToken 优先；userInfo/userJwt 是 URL 编码的 JSON。
func TestTraeParseCallback(t *testing.T) {
	userInfo := url.QueryEscape(`{"UserID":"u-1","ScreenName":"昵称","TenantID":"ent-1"}`)
	raw := "http://127.0.0.1:18080/authorize?refreshToken=rt-1&userInfo=" + userInfo
	info, err := TraeParseCallback(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.RefreshToken != "rt-1" || info.UID != "u-1" || info.Nickname != "昵称" || info.EnterpriseID != "ent-1" {
		t.Fatalf("info = %+v", info)
	}
}

// 缺 refreshToken 时回退 userJwt.RefreshToken（再兜底 userJwt.Token）。
func TestTraeParseCallbackFallbacks(t *testing.T) {
	jwt := url.QueryEscape(`{"Token":"at-1","RefreshToken":"rt-jwt","TokenExpireAt":1700000000000}`)
	info, err := TraeParseCallback("http://127.0.0.1/authorize?userJwt=" + jwt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.RefreshToken != "rt-jwt" {
		t.Fatalf("RefreshToken = %q, want rt-jwt（应回退 userJwt）", info.RefreshToken)
	}

	bare := url.QueryEscape(`{"Token":"at-2","TokenExpireAt":1700000000000}`)
	info2, err := TraeParseCallback("http://127.0.0.1/authorize?userJwt=" + bare)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info2.AccessToken != "at-2" {
		t.Fatalf("AccessToken = %q, want at-2", info2.AccessToken)
	}
	if info2.ExpiresAt != 1700000000 {
		t.Fatalf("ExpiresAt = %d, want 1700000000（毫秒归一化为秒）", info2.ExpiresAt)
	}

	if _, err := TraeParseCallback("http://127.0.0.1/authorize"); err == nil {
		t.Fatal("既无 refreshToken 又无 userJwt.Token 时应报错")
	}
}

// 全链路：start → /authorize 回调（ExchangeToken + GetUserInfo 走桩上游）→ 落盘进池
// → result 报 success。这是缺口 2 的主路径，必须一次跑通。
func TestTraeWebLoginRoundTrip(t *testing.T) {
	var (
		gotExchange bool
		gotUserInfo bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case upstream.TraeEpExchange:
			gotExchange = true
			_, _ = w.Write([]byte(`{"Result":{"Token":"at-new","RefreshToken":"rt-new","TokenExpireAt":1700000000000}}`))
		case upstream.TraeEpUserInfo:
			gotUserInfo = true
			if r.Header.Get("X-Cloudide-Token") != "at-new" {
				t.Errorf("GetUserInfo 未带换新后的 token: %q", r.Header.Get("X-Cloudide-Token"))
			}
			_, _ = w.Write([]byte(`{"Result":{"UserID":"trae-u9","ScreenName":"九号","EnterpriseID":"ent-9"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	tc := upstream.NewTraeClient()
	tc.HTTP = srv.Client()
	tc.OAuthHost = srv.URL
	p := New(Config{
		Version:    "test",
		APIKey:     "k",
		AuthDir:    dir,
		Pool:       pool.New(""),
		Upstream:   &upstream.Client{},
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(&upstream.Client{}), tc),
		// OAuth host 只能来自配置（不接受回调 query，防 token 外泄）。
		TraeApiHost: srv.URL,
	})

	// 1) start：拿登录 URL + pending id
	req := httptest.NewRequest("POST", "/panel/api/login/trae/start", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("start code=%d body=%s", rr.Code, rr.Body.String())
	}
	var started struct {
		PendingID string `json:"pending_id"`
		LoginURL  string `json:"login_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start: %v (%s)", err, rr.Body.String())
	}
	if started.PendingID == "" || !strings.Contains(started.LoginURL, "auth_callback_url") {
		t.Fatalf("start resp = %+v", started)
	}

	// 2) 回调：只回传 login_trace_id（不回 machine_id/device_id），pending 靠它反查
	pu, err := url.Parse(started.LoginURL)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	trace := pu.Query().Get("login_trace_id")
	cb := "/authorize?refreshToken=rt-0&login_trace_id=" + url.QueryEscape(trace)
	rr2 := httptest.NewRecorder()
	p.TraeAuthorizeHandler()(rr2, httptest.NewRequest("GET", cb, nil))
	if rr2.Code != 200 {
		t.Fatalf("authorize code=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if !gotExchange || !gotUserInfo {
		t.Fatalf("ExchangeToken/GetUserInfo 未都走到: exchange=%v userinfo=%v", gotExchange, gotUserInfo)
	}

	// 3) 落盘 + 进池
	raw, err := os.ReadFile(filepath.Join(dir, "trae-trae-u9.json"))
	if err != nil {
		t.Fatalf("login 未落盘 trae-<uid>.json: %v", err)
	}
	back, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if back.Provider() != auth.ProviderTrae || back.Realm() != "cn" {
		t.Fatalf("provider=%q realm=%q, want trae/cn", back.Provider(), back.Realm())
	}
	if back.AccessToken != "at-new" || back.RefreshToken != "rt-new" {
		t.Fatalf("token 未落盘: at=%q rt=%q", back.AccessToken, back.RefreshToken)
	}
	// deviceId 必须被固化（16 位数字）：签到要求每号互异，缺了后续签到会被拦。
	if len(back.DeviceID) != 16 || strings.TrimLeft(back.DeviceID, "0123456789") != "" {
		t.Fatalf("DeviceID = %q, want 16 位纯数字", back.DeviceID)
	}
	if st, ok := p.cfg.Pool.Status("trae-u9"); !ok || st.Provider != auth.ProviderTrae {
		t.Fatalf("登录账号未进池或 provider 错: %+v", st)
	}

	// 4) result：面板轮询看到 success
	rr3 := httptest.NewRecorder()
	p.ServeHTTP(rr3, authReq("GET", "/panel/api/login/trae/result?pending_id="+started.PendingID))
	if rr3.Code != 200 {
		t.Fatalf("result code=%d body=%s", rr3.Code, rr3.Body.String())
	}
	var res struct {
		State    string `json:"state"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(rr3.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.State != "success" || res.UID != "trae-u9" || res.Nickname != "九号" {
		t.Fatalf("result = %+v, want success/trae-u9/九号", res)
	}
}

// Trae provider 未启用时，登录不该被静默发给 WorkBuddy 上游（必须显式报错）。
func TestTraeLoginStartRequiresTraeProvider(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", AuthDir: t.TempDir(), Pool: pool.New(""), Upstream: &upstream.Client{}})
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, authReq("POST", "/panel/api/login/trae/start", `{}`))
	if rr.Code != 503 {
		t.Fatalf("code=%d body=%s, want 503", rr.Code, rr.Body.String())
	}
}

// 回调 uid 含路径字符必须被拒（防穿越），且不落盘。
func TestTraeAuthorizeRejectsUnsafeUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == upstream.TraeEpExchange {
			_, _ = w.Write([]byte(`{"Result":{"Token":"at-new","RefreshToken":"rt-new"}}`))
			return
		}
		w.WriteHeader(404) // GetUserInfo 缺失：uid 只能来自回调
	}))
	defer srv.Close()

	dir := t.TempDir()
	tc := upstream.NewTraeClient()
	tc.HTTP = srv.Client()
	tc.OAuthHost = srv.URL
	p := New(Config{
		Version:     "test",
		AuthDir:     dir,
		Pool:        pool.New(""),
		Upstream:    &upstream.Client{},
		Dispatcher:  upstream.NewDispatcher(upstream.NewWBClient(&upstream.Client{}), tc),
		TraeApiHost: srv.URL,
	})
	userInfo := url.QueryEscape(`{"UserID":"../evil","ScreenName":"x"}`)
	rr := httptest.NewRecorder()
	p.TraeAuthorizeHandler()(rr, httptest.NewRequest("GET", "/authorize?refreshToken=rt-0&userInfo="+userInfo, nil))
	if rr.Code != 400 {
		t.Fatalf("code=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("unsafe uid 不应落盘，got %d entries", len(entries))
	}
}

func authReq(method, target string, body ...string) *http.Request {
	var r *http.Request
	if len(body) > 0 {
		r = httptest.NewRequest(method, target, strings.NewReader(body[0]))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer k")
	return r
}
