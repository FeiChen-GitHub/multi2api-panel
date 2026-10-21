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

// traeStaticModelIDs 动态拉取失败时的静态兜底：Trae 3.4.4 前端真实
// 可见的 6 个聊天模型（solo_agent 过滤后的可调用集）。旧表（32 条，含
// Doubao-Seed-2.1-Pro / kimi-k3 / sagitta / aquila / custom_model_* 等）来自 0.1.52 时代的 trae2api，在 3.4.4
// 下已不可见或是占位模板、未经授权验证不可调用，故替换。
var traeStaticModelIDs = []string{
	"Doubao-Seed-Code",
	"glm-5.2",
	"DeepSeek-V4-Flash-Official",
	"DeepSeek-V4-Pro-Official",
	"DeepSeek-V4-Pro",
	"minimax-m3",
	"qwen-3.7-plus",
}

// traeStaticModelInfos 把静态 ID 列表转成 ModelInfo（context 沿用 trae2api 的 131072）。
func traeStaticModelInfos() []ModelInfo {
	out := make([]ModelInfo, 0, len(traeStaticModelIDs))
	for _, id := range traeStaticModelIDs {
		out = append(out, ModelInfo{ID: id, ContextWindow: 131072})
	}
	return out
}

// traeNonChatConfigs 不是聊天模型的服务配置（标题生成 / 输入优化 /
// 摘要 / 快速 apply / 搜索 agent 等），拉取时过滤。这些 config_name 在
// 3.4.4 solo_agent 响应中出现，但不是可对话模型。
//
// 注意：get_detail_param 返回的是后端全量配置注册表（solo_agent 44、chat_v3 42），
// 而 Trae 3.4.4 前端实际可见的 solo_agent 只有 21 条（其中 14 条是 custom_model_*)。
// 隐藏项都带 is_invisible_to_user=true（advisor / -auto / doubao-for-auto / glm-4.7 等内部
// 路由变体）。过滤后剩下的才是用户能看到能选的真模型。
var traeNonChatConfigs = map[string]bool{
	"title_generation":           true,
	"input_optimization":         true,
	"summary":                    true,
	"fast_apply":                 true,
	"fast_apply_new":             true,
	"browser_use_subagent":       true,
	"explore_sub_agent_v13":      true,
	"explore_sub_agent_v2":       true,
	"search_agent_qwen_fast":     true,
	"search_agent_qwen_fast-0710": true,
}

// traeModelConfigEntry 是 get_detail_param 响应 config_info_list 的条目结构。
// 独立声明，便于 solo_agent / chat_v3 两个来源共用解析。
type traeModelConfigEntry struct {
	ConfigName          string `json:"config_name"`
	IsDefault           bool   `json:"is_default"`
	// IsInvisibleToUser 后端标记为隐藏（advisor / -auto / for-auto 等内部路由变体）。
	// 3.4.4 前端会过滤这些，只显示 inv != true 的。注意：字段缺失为 nil
	// （未设置隐藏），返回 json 里存在为 true / false；故用 *bool 。
	IsInvisibleToUser  *bool  `json:"is_invisible_to_user"`
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
}

// traeParseModelList 把一个 function 的 get_detail_param 响应解析为 ModelInfo
// 列表，过滤非聊天服务配置。解析失败返回 nil（由调用方决定是否回退）。
func traeParseModelList(raw []byte) []ModelInfo {
	var out struct {
		ConfigInfoList []traeModelConfigEntry `json:"config_info_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	infos := make([]ModelInfo, 0, len(out.ConfigInfoList))
	for _, cfg := range out.ConfigInfoList {
		id := strings.TrimSpace(cfg.ConfigName)
		if id == "" || traeNonChatConfigs[id] {
			continue
		}
		// 隐藏配置（is_invisible_to_user=true）过滤：这些是 advisor / -auto /
		// for-auto 等内部路由变体，Trae 3.4.4 前端不向用户显示，网关也不应暴露。
		if cfg.IsInvisibleToUser != nil && *cfg.IsInvisibleToUser {
			continue
		}
		// custom_model_* 是用户自绱 API key 的模板槽位（前端可见但未配置时
		// 调用会失败），对 OpenAI 兼容网关无意义，过滤掉。
		if strings.HasPrefix(id, "custom_model_") {
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
	return infos
}

// traeFetchModelConfigs 请求单个 function 的 get_detail_param，返回解析后的
// ModelInfo 列表。失败（网络/非 2xx/解析失败）返回 nil。
func (c *TraeClient) traeFetchModelConfigs(ctx context.Context, a *auth.Auth, function string) []ModelInfo {
	payload, _ := json.Marshal(map[string]any{
		"function":            function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AgentHost+TraeEpModels, bytes.NewReader(payload))
	if err != nil {
		return nil
	}
	traeModelHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil
	}
	return traeParseModelList(raw)
}

// FetchModels 拉 SOLO 模型表（get_detail_param）：3.4.4 头下以 solo_agent
// 为主、chat_v3 为补充，按 config_name 去重。全部失败回落静态表且不返回错误
// —— 与 trae2api 的 modelList 回退语义一致。
//
// 背景：3.4.4 的 solo_work_lite 只返回 15 条旧配置，详见 README 。solo_agent 返回 44
// 条真实 agent 模型，chat_v3 返回 42 条，二者高度重叠但 chat_v3 多出 kimi-k2 / qwen3-coder
// 等 solo_agent 缺的条目，合并后覆盖最全。对话端点仍用 0.1.52 头（见 traeSOLOHeaders），此处
// 模型发现独立用 3.4.4 头（traeModelHeaders），互不影响。
func (c *TraeClient) FetchModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	infos := c.traeFetchModelConfigs(ctx, a, TraeModelListFunction)
	if alt := c.traeFetchModelConfigs(ctx, a, TraeModelListFunctionAlt); alt != nil {
		infos = traeMergeModels(infos, alt)
	}
	if len(infos) == 0 {
		return traeStaticModelInfos(), nil
	}
	return infos, nil
}

// traeMergeModels 按 ID 去重合并两个来源：已存在的保留，第二个
// 来源中的新条目补在后面。严格匹配 ID（区分大小写）。
func traeMergeModels(primary, extra []ModelInfo) []ModelInfo {
	if len(primary) == 0 {
		return extra
	}
	seen := make(map[string]bool, len(primary))
	for _, m := range primary {
		seen[m.ID] = true
	}
	out := make([]ModelInfo, len(primary), len(primary)+len(extra))
	copy(out, primary)
	for _, m := range extra {
		if !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out
}
