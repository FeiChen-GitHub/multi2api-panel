// trae.go 面板侧的 provider 分发辅助：Trae 账号的签到 / 额度刷新走 ChatClient，
// 与 WorkBuddy 的专用端点区分开（DESIGN §11.1）。
package panel

import (
	"context"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// clientFor 返回处理该 provider 的上游客户端；Dispatcher 未注入或未接入时
// 退化为 WorkBuddy 客户端（测试/裸用场景行为不变）。
func (p *Panel) clientFor(provider auth.Provider) upstream.ChatClient {
	if p.cfg.Dispatcher != nil {
		if c := p.cfg.Dispatcher.For(provider); c != nil {
			return c
		}
	}
	return upstream.NewWBClient(p.cfg.Upstream)
}

// checkinTrae 单号 Trae 签到：Checkin（status → 未签到则 claim）+ QueryCredits 刷新额度。
// 响应字段与 WorkBuddy 单号签到保持一致（checkin_done / checkin_message / credits / credits_total）。
func (p *Panel) checkinTrae(w http.ResponseWriter, a *auth.Auth) {
	ctx := context.Background()
	client := p.clientFor(auth.ProviderTrae)
	resp := map[string]any{"ok": true, "provider": string(auth.ProviderTrae)}

	if err := client.Checkin(ctx, a); err != nil {
		// 9074「当前使用人数太多」等拥挤错误：如实回报，不罚号。
		resp["checkin_message"] = err.Error()
	} else {
		p.cfg.Pool.NoteCheckinDone(a.UID)
		resp["checkin_done"] = true
	}

	remain, total, err := client.QueryCredits(ctx, a)
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(a.UID, remain, total)
	p.cfg.Pool.SetCredits(a.UID, remain, total)
	resp["credits"] = remain
	resp["credits_total"] = total
	writeJSON(w, http.StatusOK, resp)
}
