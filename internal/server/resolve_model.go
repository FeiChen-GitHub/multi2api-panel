package server

import (
	"errors"
	"fmt"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ErrProviderRequired 模型名缺少 provider 前缀，或 provider 未知 / 模型名为空。
var ErrProviderRequired = errors.New("model must be provider-qualified")

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

// resolveModel 解析 provider:[realm:]model（多 provider 路由协议，DESIGN §3.1）。
//
// provider 必填：缺 provider / 未知 provider / 空模型名 → ErrProviderRequired（handler 回 400）。
// 段序固定 provider 在前、realm 在后；realm 缺省 cn。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要）。
func resolveModel(model string) (provider auth.Provider, realm, bare string, err error) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", "", "", ErrProviderRequired
	}
	p, ok := providerAlias(model[:idx])
	if !ok {
		return "", "", "", fmt.Errorf("%w: unknown provider %q", ErrProviderRequired, model[:idx])
	}
	provider, rest := p, model[idx+1:]
	realm = "cn"
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		if seg := rest[:i]; seg == "cn" || seg == "global" {
			realm, rest = seg, rest[i+1:]
		}
	}
	if rest == "" {
		return "", "", "", fmt.Errorf("%w: empty model name", ErrProviderRequired)
	}
	return provider, realm, rest, nil
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (auth.Provider, string, string, error) { return resolveModel(model) }
