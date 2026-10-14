package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// get_detail_param 的动态模型表：config_info_list[].config_name 就是 config_name。
func TestTraeFetchModelsParsesDynamicList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpModels {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"config_info_list":[` +
			`{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}},` +
			`{"config_name":"DeepSeek-V4-Flash-Official","display_config":{"display_name":"DeepSeek Flash"}}]}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 2 || infos[0].ID != "glm-5.2" || infos[1].ID != "DeepSeek-V4-Flash-Official" {
		t.Fatalf("infos = %+v", infos)
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
