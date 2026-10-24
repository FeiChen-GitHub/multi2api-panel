package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Trae 3.4.4 的 UI 模型缓存（solo_agent）是权威目录：21 个可选模型，并额外追加 Auto。
// 目录来自安装包渲染进程缓存，而不是 get_detail_param 的全量配置注册表。
func TestTraeFetchModelsReturns344Catalog(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	c.ModelCachePath = filepath.Join(t.TempDir(), "missing.vscdb")

	infos, err := c.FetchModels(context.Background(), &auth.Auth{AccessToken: "jwt", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("FetchModels hit upstream %d time(s), want 0 (3.4.4 catalog is local)", got)
	}
	if len(infos) != 22 {
		t.Fatalf("catalog len = %d, want 21 static + Auto: %+v", len(infos), infos)
	}

	wantIDs := []string{
		"Doubao-Seed-2.1-Turbo",
		"Doubao-Seed-2.1-Pro",
		"Doubao-Seed-Evolving",
		"Doubao-Seed-Code",
		"glm-5.3-flash",
		"glm-5.3-flashx",
		"glm-5.3",
		"glm-5.2",
		"deepseek-v4.1-flash",
		"DeepSeek-V4-Flash-Official",
		"DeepSeek-V4-Pro-Official",
		"kimi-k3",
		"kimi-k2.8-preview",
		"mimo-v2.6-flash",
		"mimo-v2.6-pro",
		"minimax-m3.1-flash",
		"minimax-m3",
		"step-5-preview",
		"qwen3.8-flash",
		"qwen3.8-max",
		"qwen-3.7-plus",
	}
	byID := make(map[string]ModelInfo, len(infos))
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	if len(byID) != len(wantIDs)+1 {
		t.Fatalf("unique ids = %d, want %d", len(byID), len(wantIDs)+1)
	}
	for _, id := range wantIDs {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing model %q", id)
		}
	}
	for _, id := range []string{
		"auto", "custom_model_gemini", "glm-5.1", "minimax-m2",
		"kimi-k2-0905", "Doubao_1_6", "doubao-for-auto",
	} {
		if _, ok := byID[id]; ok {
			t.Errorf("legacy/internal model %q must not be exposed", id)
		}
	}

	glm := byID["glm-5.3"]
	if glm.Name != "GLM-5.3" || glm.ContextWindow != 200000 || glm.MaxTokens != 64000 {
		t.Errorf("glm-5.3 base fields = %+v", glm)
	}
	if !glm.SupportsReasoning || len(glm.Efforts) != 3 || glm.DefaultEffort != "extra_high" {
		t.Errorf("glm-5.3 reasoning = %+v", glm)
	}
	if glm.Credits != "x0.78" || glm.SupportsImages {
		t.Errorf("glm-5.3 credits/images = %q/%v", glm.Credits, glm.SupportsImages)
	}

	qwen := byID["qwen-3.7-plus"]
	if qwen.SupportsReasoning || len(qwen.Efforts) != 0 || qwen.DefaultEffort != "" {
		t.Errorf("qwen-3.7-plus must not advertise thinking: %+v", qwen)
	}
	if !qwen.SupportsImages || qwen.Credits != "x0.25" {
		t.Errorf("qwen-3.7-plus capabilities = %+v", qwen)
	}

	if seed := byID["Doubao-Seed-Code"]; !seed.IsDefault || seed.ContextWindow != 256000 || seed.MaxTokens != 32000 || seed.Credits != "x0.06" {
		t.Errorf("Doubao-Seed-Code = %+v", seed)
	}
}

// 调用方修改返回切片不能污染下一次模型列表。
func TestTraeStaticModelInfosReturnsCopy(t *testing.T) {
	first := traeStaticModelInfos()
	first[0].ID = "mutated"
	first[0].Efforts[0] = "mutated"
	second := traeStaticModelInfos()
	if second[0].ID == "mutated" || second[0].Efforts[0] == "mutated" {
		t.Fatalf("static catalog leaked mutable state: %+v", second[0])
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
