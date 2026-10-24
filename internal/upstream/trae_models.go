// trae_models.go TRAE 模型目录：优先读取 Trae 本地 state.vscdb 缓存，
// 缓存缺失/损坏时回落到代码内置的 21 个模型，并始终额外暴露 Auto 模式。
package upstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

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

// DefaultTraeModelCachePath 返回 Trae 桌面端保存模型列表的 SQLite 路径。
// Windows: %AppData%\Trae CN\User\globalStorage\state.vscdb。
func DefaultTraeModelCachePath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "Trae CN", "User", "globalStorage", "state.vscdb")
}

// traeStaticModels 是 Trae 3.4.4 solo_agent 的固定目录。字段来自 Trae 本地缓存；
// 仅作为本地缓存不可用时的兜底，不在运行时访问任何 Trae RPC。
var traeStaticModels = []ModelInfo{
	{ID: "Doubao-Seed-2.1-Turbo", Name: "Seed-2.1-Turbo", ContextWindow: 256000, MaxTokens: 32000, Credits: "x0.2", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
	{ID: "Doubao-Seed-2.1-Pro", Name: "Seed-2.1-Pro-0915", ContextWindow: 256000, MaxTokens: 64000, Credits: "x0.8", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
	{ID: "Doubao-Seed-Evolving", Name: "Seed-Evolving", ContextWindow: 256000, MaxTokens: 64000, Credits: "x0.8", SupportsImages: true},
	{ID: "Doubao-Seed-Code", Name: "Seed-Code", ContextWindow: 256000, MaxTokens: 32000, Credits: "x0.06", IsDefault: true, SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.08", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
	{ID: "glm-5.3-flashx", Name: "GLM-5.3-FlashX", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.31", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
	{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.78", SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
	{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.78", SupportsReasoning: true, Efforts: []string{"high", "extra_high"}, DefaultEffort: "high"},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.12", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
	{ID: "DeepSeek-V4-Flash-Official", Name: "DeepSeek-V4-Flash", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.2", SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
	{ID: "DeepSeek-V4-Pro-Official", Name: "DeepSeek-V4-Pro", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.72", SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
	{ID: "kimi-k3", Name: "Kimi-K3", ContextWindow: 200000, MaxTokens: 64000, Credits: "x1.83", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.98", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
	{ID: "mimo-v2.6-flash", Name: "MiMo-V2.6-Flash", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.06", SupportsImages: true},
	{ID: "mimo-v2.6-pro", Name: "MiMo-V2.6-Pro", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.15", SupportsImages: true},
	{ID: "minimax-m3.1-flash", Name: "MiniMax-M3.1-Flash", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.09", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "extra_high"},
	{ID: "minimax-m3", Name: "MiniMax-M3", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.26", SupportsImages: true},
	{ID: "step-5-preview", Name: "Step-5-Preview", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.48", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high"}, DefaultEffort: "high"},
	{ID: "qwen3.8-flash", Name: "Qwen3.8-Flash", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.08", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
	{ID: "qwen3.8-max", Name: "Qwen3.8-Max", ContextWindow: 200000, MaxTokens: 64000, Credits: "x1.5", SupportsImages: true, SupportsReasoning: true, Efforts: []string{"light", "high", "extra_high"}, DefaultEffort: "high"},
	{ID: "qwen-3.7-plus", Name: "Qwen3.7-Plus", ContextWindow: 200000, MaxTokens: 64000, Credits: "x0.25", SupportsImages: true},
}

// traeStaticModelInfos 返回静态目录副本，避免调用方修改包级数据。
func traeStaticModelInfos() []ModelInfo {
	out := make([]ModelInfo, len(traeStaticModels))
	for i, m := range traeStaticModels {
		out[i] = cloneTraeModelInfo(m)
	}
	return out
}

func cloneTraeModelInfo(m ModelInfo) ModelInfo {
	m.Efforts = append([]string(nil), m.Efforts...)
	m.Tags = append([]string(nil), m.Tags...)
	return m
}

// traeCachedModel Trae state.vscdb 中 model_list_map.solo_agent 的条目形态。
type traeCachedModel struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	IsDefault         bool   `json:"is_default"`
	Multimodal        bool   `json:"multimodal"`
	ModelType         string `json:"model_type"`
	ContextWindowSize struct {
		Max     []int64 `json:"max"`
		Default int64   `json:"default"`
	} `json:"context_window_size"`
	MaxTokens int64 `json:"max_tokens"`
	Features  struct {
		ConsumptionRate struct {
			Data struct {
				Rate any `json:"rate"`
			} `json:"data"`
		} `json:"consumption_rate"`
	} `json:"features"`
	ReasoningEffortConfig struct {
		SupportThinking bool     `json:"support_thinking"`
		Options         []string `json:"options"`
		DefaultLevel    string   `json:"default_level"`
	} `json:"reasoning_effort_config"`
	ProviderOfflineStatus string `json:"provider_offline_status"`
	Status                *bool  `json:"status"`
	Selectable            *bool  `json:"selectable"`
}

// traeParseCachedModels 把 model_list_map JSON 转成 ModelInfo；无效条目跳过。
func traeParseCachedModels(raw []byte) []ModelInfo {
	var root struct {
		SoloAgent []traeCachedModel `json:"solo_agent"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	out := make([]ModelInfo, 0, len(root.SoloAgent))
	seen := make(map[string]bool, len(root.SoloAgent))
	for _, m := range root.SoloAgent {
		id := strings.TrimSpace(m.Name)
		if id == "" || seen[id] || strings.EqualFold(id, "auto") {
			continue
		}
		if m.Status != nil && !*m.Status {
			continue
		}
		if m.Selectable != nil && !*m.Selectable {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(m.ProviderOfflineStatus), "hard") {
			continue
		}
		ctx := m.ContextWindowSize.Default
		if ctx <= 0 && len(m.ContextWindowSize.Max) > 0 {
			ctx = m.ContextWindowSize.Max[0]
		}
		name := strings.TrimSpace(m.DisplayName)
		if name == "" {
			name = id
		}
		efforts := append([]string(nil), m.ReasoningEffortConfig.Options...)
		defaultEffort := strings.TrimSpace(m.ReasoningEffortConfig.DefaultLevel)
		if !containsEffort(efforts, defaultEffort) {
			defaultEffort = ""
		}
		out = append(out, ModelInfo{
			ID:                id,
			Name:              name,
			ContextWindow:     ctx,
			MaxTokens:         m.MaxTokens,
			Credits:           traeRateString(m.Features.ConsumptionRate.Data.Rate),
			IsDefault:         m.IsDefault,
			SupportsImages:    m.Multimodal,
			SupportsReasoning: m.ReasoningEffortConfig.SupportThinking,
			Efforts:           efforts,
			DefaultEffort:     defaultEffort,
		})
		seen[id] = true
	}
	return out
}

func traeRateString(v any) string {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case json.Number:
		n, _ = x.Float64()
	case string:
		n, _ = strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(x), "x"), 64)
	default:
		return ""
	}
	if n <= 0 {
		return ""
	}
	return "x" + strconv.FormatFloat(n, 'f', -1, 64)
}

// traeReadCachedModels 从 state.vscdb 读取当前账号优先的 model_list_map。
// 无匹配 UID 时依次尝试 undefined 缓存和任意有效缓存；任何错误都返回 nil。
func traeReadCachedModels(path, uid string) []ModelInfo {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key, value FROM ItemTable WHERE key LIKE '%AI.agent.model.model_list_map'`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type candidate struct {
		key   string
		value []byte
	}
	var all []candidate
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		all = append(all, candidate{key: key, value: value})
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	order := func(c candidate) int {
		switch {
		case uid != "" && (strings.HasPrefix(c.key, uid+":") || strings.HasPrefix(c.key, uid+"_")):
			return 0
		case strings.HasPrefix(c.key, "undefined_"):
			return 1
		default:
			return 2
		}
	}
	// 按优先级逐条解析，避免选中的损坏缓存遮蔽后面的有效缓存。
	for priority := 0; priority <= 2; priority++ {
		for _, c := range all {
			if order(c) != priority {
				continue
			}
			if infos := traeParseCachedModels(c.value); len(infos) > 0 {
				return infos
			}
		}
	}
	return nil
}

func traeModelsWithAuto(infos []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(infos)+1)
	for _, m := range infos {
		if strings.EqualFold(strings.TrimSpace(m.ID), "auto") {
			continue
		}
		out = append(out, cloneTraeModelInfo(m))
	}
	out = append(out, ModelInfo{ID: "Auto", Name: "Auto"})
	return out
}

// FetchModels 返回本地缓存中的 solo_agent 模型；缓存不可用时返回固定 21 个模型。
// 两种情况都在末尾追加 Auto，供网关以 trae:Auto 暴露给调用方。
func (c *TraeClient) FetchModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	_ = ctx
	uid := ""
	if a != nil {
		uid = a.UID
	}
	path := DefaultTraeModelCachePath()
	if c.ModelCachePath != "" {
		path = c.ModelCachePath
	}
	if infos := traeReadCachedModels(path, uid); len(infos) > 0 {
		return traeModelsWithAuto(infos), nil
	}
	return traeModelsWithAuto(traeStaticModelInfos()), nil
}
