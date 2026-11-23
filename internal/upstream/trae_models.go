// trae_models.go TRAE 模型目录：在线从 get_detail_param(solo_agent) 拉取，
// 按前端可见性字段过滤出可选模型，并始终追加 Auto。
// 不再读取 Trae 桌面端 state.vscdb 本地缓存，也不内置静态表。
// 拉取结果做进程内 TTL 缓存；失败时回退上次成功结果，冷启动失败仅返回 Auto。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// traeModelCacheTTL 成功拉取的模型目录在进程内缓存时长；期内 /v1/models 不再打上游。
const traeModelCacheTTL = 5 * time.Minute

// traeModelCache 进程内模型目录缓存。key 为账号指纹（uid+token 摘要），
// 避免多账号互相污染；value 为已过滤、已追加 Auto 的列表。
var traeModelCache = struct {
	sync.Mutex
	entries map[string]traeModelCacheEntry
}{entries: make(map[string]traeModelCacheEntry)}

type traeModelCacheEntry struct {
	models    []ModelInfo
	fetchedAt time.Time
}

// traeAuthFingerprint 生成账号指纹。只取 token 末 12 字符，避免把完整凭证写进日志。
func traeAuthFingerprint(a *auth.Auth) string {
	if a == nil {
		return "anonymous"
	}
	tail := a.AccessTokenValue()
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	return a.UID + "|" + tail
}

// TraeChannelForModel 判定模型走哪条通道。口径对照 trae2api 的 isWorkModel：
// 小写含 "work"，或原名含 "Flash-Official" → Work 通道；其余 SOLO。
func TraeChannelForModel(id string) Channel {
	if strings.Contains(strings.ToLower(strings.TrimSpace(id)), "work") ||
		strings.Contains(id, "Flash-Official") {
		return ChannelWork
	}
	return ChannelSOLO
}

// ---- 在线 get_detail_param 响应解析 ----

// traeDetailResponse 是 get_detail_param 的响应信封。
// 实测响应顶层即含 config_info_list（无 data 包裹），这里兼容两种形态。
type traeDetailResponse struct {
	ConfigInfoList []traeDetailConfig `json:"config_info_list"`
	Data           struct {
		ConfigInfoList []traeDetailConfig `json:"config_info_list"`
	} `json:"data"`
}

type traeDetailConfig struct {
	ConfigName    string `json:"config_name"`
	Usage         string `json:"usage"`
	ConfigSwitch  *bool  `json:"config_switch"`
	Invisible     bool   `json:"is_invisible_to_user"`
	IsDefault     bool   `json:"is_default"`
	ContextTokens struct {
		Dev int64 `json:"dev"`
		Max int64 `json:"max"`
	} `json:"context_window_tokens"`
	Display struct {
		DisplayName     string `json:"display_name"`
		Multimodal      bool   `json:"multimodal"`
		ModelCapability string `json:"model_capability"`
		IsCustomModel   bool   `json:"is_custom_model"`
	} `json:"display_config"`
	// display_contact_config 是 JSON 字符串，内含 consumption_rate.data.rate（积分倍率）。
	DisplayContactConfig string `json:"display_contact_config"`
	Reasoning            struct {
		SupportThinking bool     `json:"support_thinking"`
		Options         []string `json:"options"`
		DefaultLevel    string   `json:"default_level"`
	} `json:"reasoning_effort_config"`
	ModelDetailList []struct {
		MaxTokens       int64 `json:"max_tokens"`
		PromptMaxTokens int64 `json:"prompt_max_tokens"`
	} `json:"model_detail_list"`
}

// traeConsumptionRate 从 display_contact_config 的 JSON 字符串里取积分倍率。
func traeConsumptionRate(raw string) float64 {
	if raw == "" {
		return 0
	}
	var cfg struct {
		ConsumptionRate struct {
			Data struct {
				Rate float64 `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
	}
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return 0
	}
	return cfg.ConsumptionRate.Data.Rate
}

// traeConfigCallable 按前端可见性规则判定一条配置是否可作为可选模型。
// 真机验证：74 条原始配置 → 三重过滤 → 20 条可见集，两账号一致。
func traeConfigCallable(c traeDetailConfig) bool {
	name := strings.TrimSpace(c.ConfigName)
	if name == "" || strings.EqualFold(name, "auto") {
		return false
	}
	// 自定义模型、内部/辅助配置（title_generation/summary/search_agent 等）usage 非 chat_completion。
	if !strings.EqualFold(c.Usage, "chat_completion") {
		return false
	}
	// 前端隐藏的内部变体（-auto、_advisor、glm-5.1、minimax-m2 等）。
	if c.Invisible {
		return false
	}
	// 显式关闭的配置。
	if c.ConfigSwitch != nil && !*c.ConfigSwitch {
		return false
	}
	if c.Display.IsCustomModel {
		return false
	}
	// custom_model_* 前缀一律排除（即便 usage 伪装成 chat_completion）。
	if strings.HasPrefix(strings.ToLower(name), "custom_model_") {
		return false
	}
	return true
}

// traeParseDetailModels 把 get_detail_param 响应解析为可见模型列表（不含 Auto）。
func traeParseDetailModels(raw []byte) ([]ModelInfo, error) {
	var resp traeDetailResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode get_detail_param: %w", err)
	}
	list := resp.ConfigInfoList
	if len(list) == 0 {
		list = resp.Data.ConfigInfoList
	}
	out := make([]ModelInfo, 0, len(list))
	for _, c := range list {
		if !traeConfigCallable(c) {
			continue
		}
		info := ModelInfo{
			ID:                c.ConfigName,
			Name:              strings.TrimSpace(c.Display.DisplayName),
			ContextWindow:     c.ContextTokens.Dev,
			IsDefault:         c.IsDefault,
			SupportsImages:    c.Display.Multimodal,
			SupportsReasoning: c.Display.ModelCapability == "reasoning_model" || c.Reasoning.SupportThinking,
			Efforts:           append([]string(nil), c.Reasoning.Options...),
			DefaultEffort:     c.Reasoning.DefaultLevel,
		}
		if info.Name == "" {
			info.Name = c.ConfigName
		}
		if len(c.ModelDetailList) > 0 {
			info.MaxTokens = c.ModelDetailList[0].MaxTokens
			if info.MaxTokens == 0 {
				info.MaxTokens = c.ModelDetailList[0].PromptMaxTokens
			}
		}
		if rate := traeConsumptionRate(c.DisplayContactConfig); rate != 0 {
			info.Credits = "x" + strconv.FormatFloat(rate, 'f', -1, 64)
		}
		out = append(out, info)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("get_detail_param returned no callable models")
	}
	return out, nil
}

// fetchTraeModelsOnline 从 get_detail_param(solo_agent) 在线拉取模型目录。
func (c *TraeClient) fetchTraeModelsOnline(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	if a == nil || strings.TrimSpace(a.AccessTokenValue()) == "" {
		return nil, fmt.Errorf("missing Trae access token")
	}
	body := map[string]interface{}{
		"function":                  "solo_agent",
		"agent_type":                "",
		"current_config_info":       map[string]interface{}{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	host := strings.TrimRight(c.AgentHost, "/")
	if host == "" {
		host = TraeAgentHost
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+TraeEpModels, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	traeModelHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get_detail_param: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read get_detail_param: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get_detail_param HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return traeParseDetailModels(raw)
}

func traeModelsWithAuto(infos []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(infos)+1)
	for _, m := range infos {
		if strings.EqualFold(strings.TrimSpace(m.ID), "auto") {
			continue
		}
		out = append(out, m)
	}
	out = append(out, ModelInfo{ID: "Auto", Name: "Auto"})
	return out
}

// FetchModels 返回在线拉取的前端可见模型，末尾追加 Auto。
// 成功结果按账号缓存 5 分钟；失败回退上次成功结果；冷启动失败仅返回 Auto（不报错，保 /v1/models 可用）。
func (c *TraeClient) FetchModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	fp := traeAuthFingerprint(a)

	// 命中未过期缓存直接返回（返回副本，避免调用方修改污染缓存）。
	traeModelCache.Lock()
	if entry, ok := traeModelCache.entries[fp]; ok && time.Since(entry.fetchedAt) < traeModelCacheTTL {
		cached := append([]ModelInfo(nil), entry.models...)
		traeModelCache.Unlock()
		return cached, nil
	}
	traeModelCache.Unlock()

	infos, err := c.fetchTraeModelsOnline(ctx, a)
	if err == nil {
		withAuto := traeModelsWithAuto(infos)
		traeModelCache.Lock()
		traeModelCache.entries[fp] = traeModelCacheEntry{models: withAuto, fetchedAt: time.Now()}
		traeModelCache.Unlock()
		return append([]ModelInfo(nil), withAuto...), nil
	}

	// 拉取失败：回退上次成功结果（哪怕已过期）。
	traeModelCache.Lock()
	stale, ok := traeModelCache.entries[fp]
	traeModelCache.Unlock()
	if ok && len(stale.models) > 0 {
		return append([]ModelInfo(nil), stale.models...), nil
	}

	// 冷启动失败：仅返回 Auto，不让 /v1/models 报错。
	return traeModelsWithAuto(nil), nil
}
