package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func newTestTraeClient(srv *httptest.Server) *TraeClient {
	c := NewTraeClient()
	c.HTTP = srv.Client()
	c.StreamHTTP = srv.Client()
	c.AgentHost = srv.URL
	c.UgHost = srv.URL
	c.OAuthHost = srv.URL
	return c
}

// ChatStream 必须打 SOLO 端点、带 Cloud-IDE-JWT 与设备头、请求体经 TraePrepareBody 改写。
func TestTraeChatStreamSendsSOLORequest(t *testing.T) {
	var gotPath, gotAuth, gotMachine, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMachine = r.Header.Get("X-Machine-Id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"))
	}))
	defer srv.Close()

	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt-1", UID: "u1", MachineID: "m-1", DeviceID: "d-1"}
	rc, status, body, err := c.ChatStream(context.Background(), a,
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{}, ChannelSOLO)
	if err != nil || status != 200 {
		t.Fatalf("ChatStream status=%d err=%v body=%s", status, err, body)
	}
	defer rc.Close()

	if gotPath != TraeEpChat {
		t.Fatalf("path = %q, want %q", gotPath, TraeEpChat)
	}
	if !strings.HasPrefix(gotAuth, "Cloud-IDE-JWT ") {
		t.Fatalf("Authorization = %q, want Cloud-IDE-JWT prefix", gotAuth)
	}
	if gotMachine != "m-1" {
		t.Fatalf("X-Machine-Id = %q, want m-1", gotMachine)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("request body not JSON: %v (%s)", err, gotBody)
	}
	if sent["function"] != TraeFunction || sent["stream"] != true || sent["config_name"] != "glm-5.2" {
		t.Fatalf("SOLO body not rewritten: %s", gotBody)
	}
}

// 上游非 2xx 时返回 status + 原始 body，由上层用 TraeClassify 分类。
func TestTraeChatStreamPassthroughUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"code":4011,"message":"too many"}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt-1", UID: "u1"}
	rc, status, body, err := c.ChatStream(context.Background(), a, []byte(`{"model":"glm-5.2"}`), "", ChatMeta{}, ChannelSOLO)
	if err != nil {
		t.Fatalf("transport error should be nil for HTTP-level failures: %v", err)
	}
	if rc != nil {
		t.Fatal("rc should be nil for non-2xx")
	}
	if status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
	if TraeClassify(status, string(body)) != ErrSoftRate {
		t.Fatalf("TraeClassify(429) = %v, want ErrSoftRate", TraeClassify(status, string(body)))
	}
}

func TestTraeClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, "", ErrNone},
		{200, `{"code":1005,"message":"plan"}`, ErrHardCredit},
		{200, `{"code":4008,"message":"quota"}`, ErrHardCredit},
		{401, "unauthorized", ErrSessionDead},
		{429, "rate", ErrSoftRate},
		{404, "nope", ErrNotFound},
		{500, "boom", ErrServer},
		{400, "bad", ErrClient},
	}
	for _, tc := range cases {
		if got := TraeClassify(tc.status, tc.body); got != tc.want {
			t.Errorf("TraeClassify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// 签到：status 显示未签到且 enable → 调 claim；已签到则只查不领。
func TestTraeCheckinClaimsOnlyWhenNeeded(t *testing.T) {
	var statusCalls, claimCalls int32
	checkedIn := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case TraeEpCheckinStatus:
			atomic.AddInt32(&statusCalls, 1)
			if checkedIn {
				_, _ = w.Write([]byte(`{"checked_in":true,"credits":10,"enable":true}`))
			} else {
				_, _ = w.Write([]byte(`{"checked_in":false,"credits":0,"enable":true}`))
			}
		case TraeEpCheckinClaim:
			atomic.AddInt32(&claimCalls, 1)
			checkedIn = true
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt-1", UID: "u1", DeviceID: "1234567890123456"}

	if err := c.Checkin(context.Background(), a); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if atomic.LoadInt32(&claimCalls) != 1 {
		t.Fatalf("claim calls = %d, want 1", claimCalls)
	}
	if err := c.Checkin(context.Background(), a); err != nil {
		t.Fatalf("Checkin 2: %v", err)
	}
	if atomic.LoadInt32(&claimCalls) != 1 {
		t.Fatalf("claim calls after already-checked-in = %d, want still 1", claimCalls)
	}
}

// 额度查询：把 user_entitlement_pack_list 的 credits_limit - credits_amount 聚合。
func TestTraeQueryCredits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpEntUsage {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[` +
			`{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":30.5}},` +
			`{"entitlement_base_info":{"quota":{"credits_limit":50}},"usage":{"credits_amount":10}}]}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt-1", UID: "u1"}

	remain, total, err := c.QueryCredits(context.Background(), a)
	if err != nil {
		t.Fatalf("QueryCredits: %v", err)
	}
	if total != 150 {
		t.Fatalf("total = %d, want 150", total)
	}
	// 逐包 int64 截断（与 trae2api 同口径）：(100-30) + (50-10) = 110，
	// 而不是对总和 109.5 再截断。
	if remain != 110 {
		t.Fatalf("remain = %d, want 110", remain)
	}
}

// CreditPackages / AccountCredits：真实字段名（display_desc、start_time、end_time、
// product_id）映射为有包名/来源/到期的明细，供面板「积分构成」与到期聚合使用。
func TestTraeCreditPackagesMapsRealFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpEntUsage {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"user_entitlement_pack_list":[` +
			`{"display_desc":"签到奖励","entitlement_base_info":{"product_id":208,"start_time":1791855046,"end_time":1794533446,"quota":{"credits_limit":100}},"usage":{"credits_amount":30}},` +
			`{"display_desc":"免费","entitlement_base_info":{"quota":{"credits_limit":0}},"usage":{}}]}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt-1", UID: "u1"}

	packs, remain, total, err := c.CreditPackages(context.Background(), a)
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs = %d, want 1（credits_limit<=0 的权益包跳过）", len(packs))
	}
	if packs[0].Name != "签到奖励" {
		t.Errorf("name = %q, want 签到奖励", packs[0].Name)
	}
	if packs[0].PackageCode != "trae_208" {
		t.Errorf("package_code = %q, want trae_208", packs[0].PackageCode)
	}
	if packs[0].CreatedAt == "" || packs[0].EndTime == "" {
		t.Errorf("created_at/end_time 不应为空: %+v", packs[0])
	}
	if packs[0].ExpiresAt != 1794533446*1000 {
		t.Errorf("expires_at = %d, want 毫秒时间戳", packs[0].ExpiresAt)
	}
	if remain != 70 || total != 100 {
		t.Errorf("remain/total = %d/%d, want 70/100", remain, total)
	}

	rem2, tot2, expiring, earliestAt, _, err := c.AccountCredits(context.Background(), a, 0)
	if err != nil {
		t.Fatalf("AccountCredits: %v", err)
	}
	if rem2 != 70 || tot2 != 100 {
		t.Errorf("AccountCredits remain/total = %d/%d, want 70/100", rem2, tot2)
	}
	if earliestAt.IsZero() {
		t.Error("AccountCredits earliestAt 不应为零值（包有 end_time）")
	}
	if expiring != 0 {
		t.Errorf("expiring(soon=0) = %d, want 0（soon=0 不分桶）", expiring)
	}
}

// Token 刷新：ExchangeToken 成功要写回 a（持锁），并归一化毫秒级过期时间。
func TestTraeRefreshTokenUpdatesAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpExchange {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Result":{"Token":"new-at","RefreshToken":"new-rt","TokenExpireAt":1786847930141}}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "old-at", RefreshToken: "old-rt", UID: "u1"}

	if err := c.RefreshToken(context.Background(), a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if a.AccessToken != "new-at" || a.RefreshToken != "new-rt" {
		t.Fatalf("auth not updated: %+v", a)
	}
	if a.ExpiresAt != 1786847930 {
		t.Fatalf("ExpiresAt = %d, want 1786847930 (ms → s)", a.ExpiresAt)
	}
}
