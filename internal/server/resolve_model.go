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
	provider, realm, bare, _, err = resolveModelDetail(model)
	return
}

// resolveModelDetail 在 resolveModel 基础上多返回 realmExplicit：调用方是否显式写了
// realm 段。跨域回退（issue #135）据此判定——只有「没写 realm」的请求才允许在首选域
// 选不到号时改用同 provider 的另一域；显式 wb:cn: / wb:global: 是用户的明确意图，
// 永不回退。
//
// 注意 provider 仍然必填（与 haiou 的路由协议一致）：裸模型名不是「缺省 provider」，
// 而是 400。跨域回退因此只发生在「写了 provider、没写 realm」这一种形态上。
func resolveModelDetail(model string) (provider auth.Provider, realm, bare string, realmExplicit bool, err error) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", "", "", false, ErrProviderRequired
	}
	p, ok := providerAlias(model[:idx])
	if !ok {
		return "", "", "", false, fmt.Errorf("%w: unknown provider %q", ErrProviderRequired, model[:idx])
	}
	provider, rest := p, model[idx+1:]
	realm = "cn"
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		if seg := rest[:i]; seg == "cn" || seg == "global" {
			realm, rest, realmExplicit = seg, rest[i+1:], true
		}
	}
	if rest == "" {
		return "", "", "", false, fmt.Errorf("%w: empty model name", ErrProviderRequired)
	}
	return provider, realm, rest, realmExplicit, nil
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (auth.Provider, string, string, error) { return resolveModel(model) }

// otherRealm 返回另一个选号域（"cn" ↔ "global"），供跨域回退使用。
//
// 回退**只在同 provider 内**发生：Trae 目前只有 cn（phase 1），翻到 global 自然是
// 空集，ServableForProviderModelRealmChannel 会返回 0，因此无需在这里对 provider
// 特判——语义上「该域没有可服务的号」已经足够。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}
