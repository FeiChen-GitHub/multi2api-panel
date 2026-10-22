package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 面板 /panel/api/models 的 provider 分流（回归守卫）：
// 池里只有 Trae 账号时，绝不能拿 Trae 的 access token 去打 WorkBuddy 的
// /console/enterprises/personal/models——两者 realm 都是 cn，早先的
// AvailableUIDsForRealm("cn") 不过滤 provider，纯 Trae 池必然选中 Trae 号，
// 上游 nginx 甩 401 HTML（用户实报："fetch models: cn: models api status 401"）。
//
// 本测试把 WorkBuddy 上游换成计数探针：被调用即失败。修复后该端点命中数必须为 0。
func TestPanelModelsTraeOnlyPoolSkipsWorkBuddyEndpoint(t *testing.T) {
	var wbHits int32
	wbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&wbHits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html><head><title>401 Authorization Required</title></head></html>"))
	}))
	defer wbSrv.Close()

	var traeHits int32
	traeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&traeHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config_info_list":[` +
			`{"config_name":"glm-5.2","display_config":{"display_name":"GLM-5.2"}},` +
			`{"config_name":"DeepSeek-V4-Flash-Official","display_config":{"display_name":"DeepSeek V4"}}]}`))
	}))
	defer traeSrv.Close()

	// 池里只放一个 Trae 账号（provider=trae、realm=cn，与真实导入产物同形态）。
	a, err := auth.Parse([]byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":9999999999,` +
		`"apiHost":"https://api.trae.com.cn","machineId":"m-1","deviceId":"1234567890123456"},` +
		`"account":{"uid":"trae-u1","nickname":"T"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	auth.SetProvider(a, auth.ProviderTrae)
	if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
		t.Fatalf("set realm: %v", err)
	}
	if _, _, err := a.EnsureKey(); err != nil {
		t.Fatalf("ensure key: %v", err)
	}
	pl := pool.New("")
	pl.Add(a)

	up := &upstream.Client{HTTP: wbSrv.Client(), ChatBaseCN: wbSrv.URL}
	tc := &upstream.TraeClient{HTTP: traeSrv.Client(), AgentHost: traeSrv.URL}
	p := New(Config{
		Version:    "test",
		AuthDir:    t.TempDir(),
		Pool:       pl,
		Upstream:   up,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc),
	})

	rr := httptest.NewRecorder()
	p.models(rr, httptest.NewRequest(http.MethodGet, "/panel/api/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}

	// ① WorkBuddy 端点零调用——401 的根因就在这里。
	if n := atomic.LoadInt32(&wbHits); n != 0 {
		t.Fatalf("WorkBuddy models endpoint hit %d time(s); Trae token must not be sent there", n)
	}
	// ② Trae 端点确实被查了（否则上面是"什么都没查"的假通过）。
	// 3.4.4 模型发现会请求两个 function（solo_agent + chat_v3），
	// 故 hits → 2；这里只守“必须有调用”，具体次数交给上游决定。
	if n := atomic.LoadInt32(&traeHits); n < 1 {
		t.Fatalf("Trae models endpoint hits = %d, want >= 1", n)
	}

	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if len(out.Models) != 2 {
		t.Fatalf("models = %d, want 2: %s", len(out.Models), rr.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, m := range out.Models {
		byID[m["id"].(string)] = m
	}
	entry, ok := byID["trae:glm-5.2"]
	if !ok {
		t.Fatalf("missing trae:glm-5.2 entry: %s", rr.Body.String())
	}
	// ③ id 前缀与 /v1/models 一致（wb:/wb:global:/trae:），旧面板的 "cn:" 不是合法网关模型名。
	if entry["provider"] != string(auth.ProviderTrae) || entry["realm"] != "cn" {
		t.Fatalf("entry provider/realm = %v/%v, want trae/cn", entry["provider"], entry["realm"])
	}
	// ④ 不套 WorkBuddy 的静态档位表：glm-5.2 在两边同名，套上去等于编档位数据。
	if _, bad := entry["supported_efforts"]; bad {
		t.Fatalf("Trae entry must not carry WorkBuddy effort table: %v", entry["supported_efforts"])
	}
	if _, bad := entry["credits"]; bad {
		t.Fatalf("Trae entry must not carry a rate: %v", entry["credits"])
	}
	// ⑤ 通道判定（Work / SOLO）照旧透出，供前端识别。
	if byID["trae:DeepSeek-V4-Flash-Official"]["channel"] != string(upstream.ChannelWork) {
		t.Fatalf("Flash-Official channel = %v, want work", byID["trae:DeepSeek-V4-Flash-Official"]["channel"])
	}
	if entry["channel"] != string(upstream.ChannelSOLO) {
		t.Fatalf("glm-5.2 channel = %v, want solo", entry["channel"])
	}
}

// 混池（WorkBuddy CN + Trae）时两段都要出，且各走各的上游：Trae 条目不得因
// WorkBuddy 段失败/成功而丢失，WorkBuddy 段也不得拿 Trae 号去探测。
func TestPanelModelsMixedPoolSplitsByProvider(t *testing.T) {
	var wbAuthHeaders []string
	wbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wbAuthHeaders = append(wbAuthHeaders, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		// agents 是数组（不是 map），models 是富字段数组；cli 名单 ∩ 富字段 = 输出。
		_, _ = w.Write([]byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["wb-only"]}],` +
			`"models":[{"id":"wb-only","name":"WB Only","maxOutputTokens":8192}]}}`))
	}))
	defer wbSrv.Close()
	traeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config_info_list":[{"config_name":"trae-only","display_config":{"display_name":"Trae Only"}}]}`))
	}))
	defer traeSrv.Close()

	traeAcct, err := auth.Parse([]byte(`{"auth":{"accessToken":"trae-token","refreshToken":"rt","expiresAt":9999999999},"account":{"uid":"trae-u1"}}`))
	if err != nil {
		t.Fatalf("parse trae: %v", err)
	}
	auth.SetProvider(traeAcct, auth.ProviderTrae)
	if _, err := auth.BackfillRealmFor(traeAcct, "cn"); err != nil {
		t.Fatalf("realm: %v", err)
	}
	if _, _, err := traeAcct.EnsureKey(); err != nil {
		t.Fatalf("key: %v", err)
	}
	wbAcct, err := auth.Parse([]byte(`{"auth":{"accessToken":"wb-token","refreshToken":"rt","expiresAt":9999999999},"account":{"uid":"wb-u1"}}`))
	if err != nil {
		t.Fatalf("parse wb: %v", err)
	}
	if _, err := auth.BackfillRealmFor(wbAcct, "cn"); err != nil {
		t.Fatalf("realm: %v", err)
	}
	if _, _, err := wbAcct.EnsureKey(); err != nil {
		t.Fatalf("key: %v", err)
	}
	pl := pool.New("")
	pl.Add(traeAcct)
	pl.Add(wbAcct)

	up := &upstream.Client{HTTP: wbSrv.Client(), ChatBaseCN: wbSrv.URL}
	tc := &upstream.TraeClient{HTTP: traeSrv.Client(), AgentHost: traeSrv.URL}
	p := New(Config{
		Version:    "test",
		AuthDir:    t.TempDir(),
		Pool:       pl,
		Upstream:   up,
		Dispatcher: upstream.NewDispatcher(upstream.NewWBClient(up), tc),
	})

	rr := httptest.NewRecorder()
	p.models(rr, httptest.NewRequest(http.MethodGet, "/panel/api/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`"wb:wb-only"`, `"trae:trae-only"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	// WorkBuddy 探测只能用 WorkBuddy 号的 token。
	for _, h := range wbAuthHeaders {
		if strings.Contains(h, "trae-token") {
			t.Fatalf("WorkBuddy endpoint probed with Trae token: %q", h)
		}
	}
}
