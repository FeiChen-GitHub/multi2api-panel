// chatclient.go 多 provider 的统一上游接口（DESIGN §6.1）+ 通道维度（§6.6）+ Dispatcher（§6.5）。
//
// 分层原则：ChatClient 只统一「传输形状 + 错误码 + 流」，不做格式转换也不统一请求体语义。
// 返回原始上游流（io.ReadCloser + status + body），SSE 归一化留在 handler 侧，
// 这样现有的空闲超时、keepalive、6004 帧解析、错误分类路径全部可复用。
package upstream

import (
	"context"
	"io"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Channel 是 provider 内部的通道维度（请求级，不进 AccountKey）。
// WorkBuddy 目前只有 solo；Trae 有 solo / work（DESIGN §6.6）。
type Channel string

const (
	ChannelSOLO Channel = "solo"
	ChannelWork Channel = "work"
)

// ChatClient 统一上游接口。
//
// 全部方法收 *auth.Auth：RefreshToken / Checkin 会在 a.mu 内改写凭证并原子写回，
// 传值会复制 mutex 并丢失写回。ctx 为请求级取消；WorkBuddy 的旧实现无 ctx 参数，
// 由 WBClient 适配（见下）。
type ChatClient interface {
	ChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta, ch Channel) (rc io.ReadCloser, status int, respBody []byte, err error)
	Checkin(ctx context.Context, a *auth.Auth) error
	RefreshToken(ctx context.Context, a *auth.Auth) error
	QueryCredits(ctx context.Context, a *auth.Auth) (remain, total int64, err error)
	// AccountCredits 带到期明细的聚合余额（供面板单号/后台余额刷新）。
	// 返回 (remain, total, expiring, earliestAt, earliestRemaining, err)，与
	// WorkBuddy 的 UserResourceDetailedWithExpiry 同口径；Trae 上游额度接口暂无
	// 按包到期明细，expiring/earliest 相关字段以零值返回（不影响 remain/total）。
	AccountCredits(ctx context.Context, a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error)
	// CreditPackages 套餐明细（供面板「套餐到期」视图聚合剩余天数）。
	// Trae 上游额度接口返回 entitlement 列表，尽力映射为 CreditPackage；缺到期
	// 字段时 EndTime/ExpiresAt 以零值返回（前端按「无到期」展示）。
	CreditPackages(ctx context.Context, a *auth.Auth) ([]CreditPackage, int64, int64, error)
}

// ModelLister 可选能力：按 provider 拉模型列表（供 /v1/models 按 provider 分组）。
// 只有 TraeClient 实现（WorkBuddy 走既有的 fetchDynamicModels/四级查找链）。
type ModelLister interface {
	FetchModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error)
}

// WBClient 把现有 WorkBuddy 客户端适配成 ChatClient：纯薄封装，行为与现状完全一致。
type WBClient struct{ *Client }

// NewWBClient 包装现有客户端。
func NewWBClient(c *Client) *WBClient { return &WBClient{Client: c} }

// ChatStream 忽略 ch（WorkBuddy 单通道），转调现有 ChatStreamContext。
func (c *WBClient) ChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta, ch Channel) (io.ReadCloser, int, []byte, error) {
	return c.Client.ChatStreamContext(ctx, a, body, clientIP, meta)
}

// Checkin 转调 DailyCheckin（签到 + 幂等语义由 upstream 内部处理）。
func (c *WBClient) Checkin(ctx context.Context, a *auth.Auth) error { return c.Client.DailyCheckin(a) }

// RefreshToken 转调现有 RefreshToken（持 a.mu，成功即完成写回，落盘由调用方负责）。
func (c *WBClient) RefreshToken(ctx context.Context, a *auth.Auth) error {
	return c.Client.RefreshToken(a)
}

// QueryCredits 转调 UserResource（返回 remain/total 聚合余额）。
func (c *WBClient) QueryCredits(ctx context.Context, a *auth.Auth) (int64, int64, error) {
	return c.Client.UserResource(a)
}

// AccountCredits 转调 UserResourceDetailedWithExpiry（带到期明细的聚合余额）。
func (c *WBClient) AccountCredits(ctx context.Context, a *auth.Auth, soon time.Duration) (int64, int64, int64, time.Time, int64, error) {
	return c.Client.UserResourceDetailedWithExpiry(a, soon)
}

// CreditPackages 转调现有 CreditPackages（WorkBuddy 套餐明细）。
func (c *WBClient) CreditPackages(ctx context.Context, a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	return c.Client.CreditPackages(a)
}

// Dispatcher 按 provider 分发到对应 ChatClient。
// trae 为 nil 时 For(ProviderTrae) 返回 nil —— 调用方必须显式处理「该 provider 未接入」，
// 绝不允许把 Trae 请求静默发给 WorkBuddy 客户端。
type Dispatcher struct {
	wb   ChatClient
	trae ChatClient
}

// NewDispatcher 构建分发器；trae 可为 nil（未接入）。
func NewDispatcher(wb, trae ChatClient) *Dispatcher {
	return &Dispatcher{wb: wb, trae: trae}
}

// For 返回处理该 provider 的客户端。未知 provider 回落 WorkBuddy（路由层已保证 provider 合法）。
func (d *Dispatcher) For(prov auth.Provider) ChatClient {
	if prov == auth.ProviderTrae {
		return d.trae
	}
	return d.wb
}
