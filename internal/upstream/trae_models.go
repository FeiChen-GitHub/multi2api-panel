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
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return traeStaticModelInfos(), nil
	}
	infos := make([]ModelInfo, 0, len(out.ConfigInfoList))
	for _, cfg := range out.ConfigInfoList {
		if strings.TrimSpace(cfg.ConfigName) == "" {
			continue
		}
		infos = append(infos, ModelInfo{ID: cfg.ConfigName, Name: cfg.DisplayConfig.DisplayName})
	}
	if len(infos) == 0 {
		return traeStaticModelInfos(), nil
	}
	return infos, nil
}
