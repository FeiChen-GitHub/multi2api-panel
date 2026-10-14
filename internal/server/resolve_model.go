package server

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// providerAlias 把模型名第一段映射成规范化 provider；未识别返回 ok=false。
func providerAlias(seg string) (auth.Provider, bool) {
	switch seg {
	case "wb", "workbuddy":
		return auth.ProviderWorkBuddy, true
	case "trae":
		return auth.ProviderTrae, true
	default:
		return "", false
	}
}

// resolveModel 解析 [provider:][realm:]model（多 provider 路由协议，DESIGN §3.1）。
//
// 过渡语义（provider 必填切换之前）：第一段不是已知 provider 时按 WorkBuddy 处理，
// 老的 "cn:xxx" / 裸名调用继续可用。段序固定 provider 在前、realm 在后；
// realm 缺省 cn；未知前缀整体当裸模型名（例如 "global:trae:x" 的 provider 段是
// "global"，不识别的 provider → 走裸名回落，而不是把 trae 当 provider）。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要）。
func resolveModel(model string) (provider auth.Provider, realm, bare string) {
	provider, realm, rest := auth.ProviderWorkBuddy, "cn", model
	idx := strings.IndexByte(rest, ':')
	if idx < 0 {
		return provider, realm, rest
	}
	if p, ok := providerAlias(rest[:idx]); ok {
		provider, rest = p, rest[idx+1:]
	}
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		if seg := rest[:i]; seg == "cn" || seg == "global" {
			realm, rest = seg, rest[i+1:]
		}
	}
	return provider, realm, rest
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (auth.Provider, string, string) { return resolveModel(model) }
