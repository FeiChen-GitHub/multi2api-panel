package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// get_detail_param 动态模型表：3.4.4 头下以 solo_agent 为主、chat_v3 为补，
// config_info_list[].config_name 就是 config_name。同时校验：发送的 function 为
// solo_agent 与 chat_v3，版本头为 3.4.4（对话头仍是 0.1.52，互不干扰）。
func TestTraeFetchModelsParsesDynamicList(t *testing.T) {
	var seenFns []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpModels {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Ide-Version") != traeModelIdeVersion {
			t.Errorf("model fetch version = %q, want %q", r.Header.Get("X-Ide-Version"), traeModelIdeVersion)
		}
		if r.Header.Get("X-Ide-Version-Code") != traeModelIdeVersionCode {
			t.Errorf("model fetch version code = %q, want %q", r.Header.Get("X-Ide-Version-Code"), traeModelIdeVersionCode)
		}
		b, _ := io.ReadAll(r.Body)
		var sent map[string]any
		if err := json.Unmarshal(b, &sent); err == nil {
			if fn, ok := sent["function"].(string); ok {
				seenFns = append(seenFns, fn)
			}
		}
		_, _ = w.Write([]byte(`{"config_info_list":[` +
			`{"config_name":"glm-5.2","context_window_tokens":{"dev":200000},"display_config":{"display_name":"GLM 5.2","model_capability":"reasoning_model"},"model_detail_list":[{"max_tokens":8192}]},` +
			`{"config_name":"DeepSeek-V4-Flash-Official","display_config":{"display_name":"DeepSeek Flash"}}]}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	// solo_agent + chat_v3 两来源均返回同两个 config_name，去重后仍为 2。
	if len(infos) != 2 {
		t.Fatalf("infos len = %d, want 2 (deduped): %+v", len(infos), infos)
	}
	if infos[0].ID != "glm-5.2" || infos[1].ID != "DeepSeek-V4-Flash-Official" {
		t.Fatalf("infos = %+v", infos)
	}
	if infos[0].ContextWindow != 200000 || infos[0].MaxTokens != 8192 || !infos[0].SupportsReasoning {
		t.Fatalf("glm-5.2 detail not parsed: %+v", infos[0])
	}
	// 必须请求过 solo_agent（主）与 chat_v3（补）。
	seenSolo := false
	seenChatV3 := false
	for _, fn := range seenFns {
		if fn == TraeModelListFunction {
			seenSolo = true
		}
		if fn == TraeModelListFunctionAlt {
			seenChatV3 = true
		}
	}
	if !seenSolo || !seenChatV3 {
		t.Fatalf("expected both %q and %q requests, got %v", TraeModelListFunction, TraeModelListFunctionAlt, seenFns)
	}
}

// 动态拉取失败要回落静态表（trae2api 的 staticModels 口径），不能返回空。
func TestTraeFetchModelsFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels should fall back to static list, got err: %v", err)
	}
	if len(infos) < 30 {
		t.Fatalf("static fallback len = %d, want >= 30", len(infos))
	}
	seen := map[string]bool{}
	for _, mi := range infos {
		seen[mi.ID] = true
	}
	if !seen["glm-5.2"] || !seen["DeepSeek-V4-Flash-Official"] {
		t.Fatalf("static list missing expected models: %v", seen)
	}
}

// 通道判定对照 trae2api 的 isWorkModel：含 work 或 Flash-Official 走 Work，其余 SOLO。
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

// 合并去重：主来源保留，补来源只补充新 ID，已存在的跳过。
func TestTraeMergeModelsDedup(t *testing.T) {
	primary := []ModelInfo{{ID: "glm-5.2"}, {ID: "kimi-k3"}}
	extra := []ModelInfo{{ID: "glm-5.2"}, {ID: "qwen3-coder"}, {ID: "kimi-k3"}}
	out := traeMergeModels(primary, extra)
	ids := make([]string, len(out))
	for i, m := range out {
		ids[i] = m.ID
	}
	want := []string{"glm-5.2", "kimi-k3", "qwen3-coder"}
	if len(ids) != len(want) {
		t.Fatalf("merge len = %d (%v), want %d (%v)", len(ids), ids, len(want), want)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("pos %d = %q, want %q (ids=%v)", i, ids[i], w, ids)
		}
	}
}

// 非聊天服务配置（标题生成 / 摘要 / fast_apply / 搜索 agent 等）要被过滤。
func TestTraeParseModelListFiltersNonChat(t *testing.T) {
	raw := []byte(`{"config_info_list":[` +
		`{"config_name":"glm-5.2","display_config":{}},` +
		`{"config_name":"title_generation","display_config":{}},` +
		`{"config_name":"summary","display_config":{}},` +
		`{"config_name":"fast_apply","display_config":{}},` +
		`{"config_name":"kimi-k3","display_config":{}}]}`)
	out := traeParseModelList(raw)
	ids := make([]string, 0, len(out))
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	want := []string{"glm-5.2", "kimi-k3"}
	if len(ids) != len(want) {
		t.Fatalf("filtered len = %d (%v), want %d (%v)", len(ids), ids, len(want), want)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("pos %d = %q, want %q", i, ids[i], w)
		}
	}
}

// chat_v3 独有的条目（solo_agent 没有）应合并补入。
func TestTraeFetchModelsMergesAltSourceExtras(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var sent map[string]any
		_ = json.Unmarshal(b, &sent)
		fn, _ := sent["function"].(string)
		if fn == TraeModelListFunction {
			_, _ = w.Write([]byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{}}]}`))
			return
		}
		// chat_v3 补充：重叠 + 独有
		_, _ = w.Write([]byte(`{"config_info_list":[{"config_name":"glm-5.2","display_config":{}},{"config_name":"qwen3-coder","display_config":{}}]}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range infos {
		seen[m.ID] = true
	}
	if !seen["glm-5.2"] || !seen["qwen3-coder"] {
		t.Fatalf("merge missing models: %v", seen)
	}
	if len(infos) != 2 {
		t.Fatalf("len = %d, want 2 (dedup)", len(infos))
	}
}
