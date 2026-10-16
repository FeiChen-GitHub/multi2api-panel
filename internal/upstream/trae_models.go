// trae_models.go TRAE SOLO 模型发现：get_detail_param 动态拉取 + 静态兜底。
//
// 对照 trae2api（internal/server/handler.go 的 staticModels / fetchDynamicModels，已核实）：
// 动态成功用上游 config_name 列表，失败回落 32 条静态兜底；动态失败不返回空，
// 否则 /v1/models 会缺 Trae 一侧。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TraeChannelForModel 判定模型走哪条通道。口径对照 trae2api 的 isWorkModel：
// 小写含 "work"，或原名含 "Flash-Official" → Work 通道；其余 SOLO。
func TraeChannelForModel(id string) Channel {
	if strings.Contains(strings.ToLower(strings.TrimSpace(id)), "work") ||
		strings.Contains(id, "Flash-Official") {
		return ChannelWork
	}
	return ChannelSOLO
}

// traeStaticModelIDs 动态拉取失败时的静态兜底（trae2api staticModels 的 32 条，
// 顺序按原表；第一条即 Work 通道模型）。
var traeStaticModelIDs = []string{
	"DeepSeek-V4-Flash-Official",
	"Doubao-Seed-2.1-Pro",
	"seed-code-pro-0430",
	"Doubao-Seed-2.1-Turbo",
	"Doubao-Seed-2.0-Code",
	"browser_use_subagent",
	"glm-5.2",
	"glm-5-turbo",
	"glm-5",
	"DeepSeek-V4-Pro",
	"DeepSeek-V4-Flash",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"minimax-m3",
	"qwen-3.7-plus",
	"sagitta",
	"aquila",
	"custom_model_gemini",
	"custom_model_placeholder",
	"custom_model_1M_text",
	"custom_model_1M",
	"custom_model_kimi",
	"custom_model_claude",
	"custom_model_gpt-5",
	"custom_model_no-fc",
	"custom_model_deepseek_chat",
	"custom_model_deepseek_reasoner",
	"custom_model_deepseek_v4",
	"explore_sub_agent_v13",
	"explore_sub_agent_v2",
	"summary",
}

// traeStaticModelInfos 把静态 ID 列表转成 ModelInfo（context 沿用 trae2api 的 131072）。
func traeStaticModelInfos() []ModelInfo {
	out := make([]ModelInfo, 0, len(traeStaticModelIDs))
	for _, id := range traeStaticModelIDs {
		out = append(out, ModelInfo{ID: id, ContextWindow: 131072})
	}
	return out
}

// FetchModels 拉 SOLO 模型表（get_detail_param）。动态失败（网络/非 2xx/解析失败/空表）
// 回落静态表且不返回错误 —— 与 trae2api 的 modelList 回退语义一致。
func (c *TraeClient) FetchModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	payload, _ := json.Marshal(map[string]any{
		"function":            TraeFunction,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AgentHost+TraeEpModels, bytes.NewReader(payload))
	if err != nil {
		return traeStaticModelInfos(), nil
	}
	traeSOLOHeaders(req, a, false)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return traeStaticModelInfos(), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return traeStaticModelInfos(), nil
	}
	var out struct {
		ConfigInfoList []struct {
			ConfigName string `json:"config_name"`
			IsDefault  bool   `json:"is_default"`
			// context_window_tokens 形如 {"dev":200000,"max":...}；缺失/为 null 时零值。
			ContextWindowTokens struct {
				Dev int64 `json:"dev"`
				Max int64 `json:"max"`
			} `json:"context_window_tokens"`
			DisplayConfig struct {
				DisplayName     string `json:"display_name"`
				Multimodal      bool   `json:"multimodal"`
				ModelCapability string `json:"model_capability"`
			} `json:"display_config"`
			// model_detail_list[0].max_tokens 是该模型的输出上限（input 上限见
			// prompt_max_tokens，二者之和 = context_window_tokens）。
			ModelDetailList []struct {
				MaxTokens int64 `json:"max_tokens"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return traeStaticModelInfos(), nil
	}
	infos := make([]ModelInfo, 0, len(out.ConfigInfoList))
	for _, cfg := range out.ConfigInfoList {
		id := strings.TrimSpace(cfg.ConfigName)
		if id == "" {
			continue
		}
		// 上下文窗口：优先 dev，缺失回落 max。此前整段被忽略，导致 /v1/models 的
		// Trae 条目没有 context_length，客户端只能靠猜——pi-ai/deepseek-harness
		// 会因此把请求判成「context overflow」。
		ctx := cfg.ContextWindowTokens.Dev
		if ctx == 0 {
			ctx = cfg.ContextWindowTokens.Max
		}
		var maxOut int64
		if len(cfg.ModelDetailList) > 0 {
			maxOut = cfg.ModelDetailList[0].MaxTokens
		}
		infos = append(infos, ModelInfo{
			ID:                id,
			Name:              cfg.DisplayConfig.DisplayName,
			ContextWindow:     ctx,
			MaxTokens:         maxOut,
			IsDefault:         cfg.IsDefault,
			SupportsImages:    cfg.DisplayConfig.Multimodal,
			SupportsReasoning: cfg.DisplayConfig.ModelCapability == "reasoning_model",
		})
	}
	if len(infos) == 0 {
		return traeStaticModelInfos(), nil
	}
	return infos, nil
}
