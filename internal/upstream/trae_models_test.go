package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// traeDetailFixture 构造 get_detail_param 响应样本，字段形态与真机一致。
func traeDetailFixture(configs ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"config_info_list": configs})
	return string(b)
}

func traeVisibleCfg(name, display string) map[string]any {
	return map[string]any{
		"config_name":           name,
		"usage":                 "chat_completion",
		"config_switch":         true,
		"is_invisible_to_user":  false,
		"is_default":            name == "Doubao-Seed-Code",
		"context_window_tokens": map[string]any{"dev": 200000, "max": 1000000},
		"display_config": map[string]any{
			"display_name": display, "multimodal": true, "model_capability": "reasoning_model", "is_custom_model": false,
		},
		"display_contact_config": `{"consumption_rate":{"enable":true,"data":{"rate":0.78}}}`,
		"reasoning_effort_config": map[string]any{
			"support_thinking": true, "options": []string{"light", "high", "extra_high"}, "default_level": "extra_high",
		},
		"model_detail_list": []map[string]any{{"max_tokens": 32000}},
	}
}

func resetTraeModelCache() {
	traeModelCache.Lock()
	traeModelCache.entries = make(map[string]traeModelCacheEntry)
	traeModelCache.Unlock()
}

// 在线拉取成功：三重过滤生效，字段映射正确，末尾追加 Auto。
func TestTraeFetchModelsOnlineFilterAndFields(t *testing.T) {
	resetTraeModelCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpModels {
			t.Errorf("path = %s, want %s", r.URL.Path, TraeEpModels)
		}
		if got := r.Header.Get("X-Ide-Version"); got != "0.1.52" {
			t.Errorf("model discovery header X-Ide-Version = %q, want 0.1.52", got)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["function"] != "solo_agent" || req["show_custom_model"] != true {
			t.Errorf("unexpected body: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(traeDetailFixture(
			traeVisibleCfg("glm-5.3", "GLM-5.3"),
			traeVisibleCfg("kimi-k3.1-pro", "Kimi-K3.1-Pro"),
			traeVisibleCfg("Doubao-Seed-Code", "Seed-Code"),
			// 内部隐藏变体
			map[string]any{"config_name": "glm-5.2", "usage": "chat_completion", "is_invisible_to_user": true, "config_switch": true},
			// 非 chat_completion
			map[string]any{"config_name": "title_generation", "usage": "title_generation", "config_switch": true},
			// custom 模型
			map[string]any{"config_name": "custom_model_gemini", "usage": "custom_model", "config_switch": true},
			// 显式关闭
			map[string]any{"config_name": "disabled-model", "usage": "chat_completion", "config_switch": false},
			// auto 伪模型
			map[string]any{"config_name": "auto", "usage": "chat_completion", "config_switch": true},
		)))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 4 {
		t.Fatalf("models = %d, want 3 visible + Auto: %+v", len(infos), infos)
	}
	byID := map[string]ModelInfo{}
	for _, m := range infos {
		byID[m.ID] = m
	}
	for _, bad := range []string{"glm-5.2", "title_generation", "custom_model_gemini", "disabled-model", "auto"} {
		if _, ok := byID[bad]; ok {
			t.Errorf("filtered-out model %q leaked", bad)
		}
	}
	if infos[3].ID != "Auto" {
		t.Errorf("last model = %+v, want Auto", infos[3])
	}
	glm := byID["glm-5.3"]
	if glm.Name != "GLM-5.3" || glm.ContextWindow != 200000 || glm.MaxTokens != 32000 ||
		glm.Credits != "x0.78" || !glm.SupportsReasoning || !glm.SupportsImages ||
		len(glm.Efforts) != 3 || glm.DefaultEffort != "extra_high" {
		t.Errorf("glm-5.3 fields = %+v", glm)
	}
	if seed := byID["Doubao-Seed-Code"]; !seed.IsDefault {
		t.Errorf("Doubao-Seed-Code IsDefault = false")
	}
}

// 在线失败且无历史缓存：仅返回 Auto，不报错（保 /v1/models 可用）。
func TestTraeFetchModelsColdStartFailureReturnsAutoOnly(t *testing.T) {
	resetTraeModelCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "cold"})
	if err != nil {
		t.Fatalf("FetchModels must not fail: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "Auto" {
		t.Fatalf("cold-start fallback = %+v, want [Auto]", infos)
	}
}

// 在线失败回退上次成功结果。
func TestTraeFetchModelsFallsBackToLastSuccess(t *testing.T) {
	resetTraeModelCache()
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(traeDetailFixture(traeVisibleCfg("glm-5.3", "GLM-5.3"))))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt", UID: "fallback"}

	if _, err := c.FetchModels(context.Background(), a); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	fail.Store(true)
	// 绕过 TTL：直接把缓存时间回拨，强制重新拉取以触发失败回退。
	traeModelCache.Lock()
	fp := traeAuthFingerprint(a)
	e := traeModelCache.entries[fp]
	e.fetchedAt = e.fetchedAt.Add(-traeModelCacheTTL - 1)
	traeModelCache.entries[fp] = e
	traeModelCache.Unlock()

	infos, err := c.FetchModels(context.Background(), a)
	if err != nil {
		t.Fatalf("fallback fetch: %v", err)
	}
	if len(infos) != 2 || infos[0].ID != "glm-5.3" || infos[1].ID != "Auto" {
		t.Fatalf("stale fallback = %+v, want [glm-5.3 Auto]", infos)
	}
}

// 成功结果被缓存：TTL 内不再打上游。
func TestTraeFetchModelsCachesSuccess(t *testing.T) {
	resetTraeModelCache()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(traeDetailFixture(traeVisibleCfg("glm-5.3", "GLM-5.3"))))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt", UID: "cache"}

	c.FetchModels(context.Background(), a)
	c.FetchModels(context.Background(), a)
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (TTL cache)", got)
	}
}

// 通道判定不变。
func TestTraeChannelForModel(t *testing.T) {
	cases := []struct {
		id   string
		want Channel
	}{
		{"DeepSeek-V4-Flash-Official", ChannelWork},
		{"work", ChannelWork},
		{"SomeWorkModel", ChannelWork},
		{"glm-5.2", ChannelSOLO},
		{"DeepSeek-V4-Pro", ChannelSOLO},
	}
	for _, tc := range cases {
		if got := TraeChannelForModel(tc.id); got != tc.want {
			t.Errorf("TraeChannelForModel(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// 兼容保留的 ModelCachePath 赋值不影响在线拉取。
func TestTraeModelCachePathIgnored(t *testing.T) {
	resetTraeModelCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(traeDetailFixture(traeVisibleCfg("online-model", "Online"))))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	c.ModelCachePath = strings.Join([]string{t.TempDir(), "stale.vscdb"}, "/")

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "ignore"})
	if err != nil || len(infos) != 2 || infos[0].ID != "online-model" {
		t.Fatalf("infos=%+v err=%v; ModelCachePath should be ignored", infos, err)
	}
}
