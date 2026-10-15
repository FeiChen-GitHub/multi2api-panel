// trae.go 面板侧的 provider 分发辅助：Trae 账号的签到 / 额度刷新走 ChatClient，
// 与 WorkBuddy 的专用端点区分开（DESIGN §11.1）。
package panel

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// importTrae 导入 Trae 凭证：body 为 trae2api 产出的 JSON（嵌套形或扁平形）。
//
//	POST /panel/api/import/trae
//
// 落盘为 auths/trae-<uid>.json（provider=trae、realm=cn，phase 1 只支持 cn），
// 并热加载进池。UID 走与 WorkBuddy 同一道 validImportUID 校验（防路径穿越）。
func (p *Panel) importTrae(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	a, err := auth.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "parse auth: "+err.Error())
		return
	}
	uid := strings.TrimSpace(a.UID)
	if !validImportUID(uid) {
		writeErr(w, http.StatusBadRequest, "invalid uid（只允许字母/数字/下划线/连字符）")
		return
	}
	auth.SetProvider(a, auth.ProviderTrae)
	if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
		writeErr(w, http.StatusBadRequest, "set realm: "+err.Error())
		return
	}
	if _, _, err := a.EnsureKey(); err != nil {
		writeErr(w, http.StatusInternalServerError, "gen account key: "+err.Error())
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a.FilePath = filepath.Join(p.cfg.AuthDir, "trae-"+uid+".json")
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	log.Printf("panel: import trae uid=%s（已落盘并入池）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "provider": "trae"})
}

// clientFor 返回处理该 provider 的上游客户端。注入了 Dispatcher 就按 provider 取，
// 返回 nil = 该 provider 未接入/未启用（调用方必须显式报错，绝不回落 WorkBuddy）；
// 未注入 Dispatcher（测试/裸用）时只有 WorkBuddy 可用。
func (p *Panel) clientFor(provider auth.Provider) upstream.ChatClient {
	if p.cfg.Dispatcher != nil {
		return p.cfg.Dispatcher.For(provider)
	}
	if provider == auth.ProviderTrae {
		return nil
	}
	return upstream.NewWBClient(p.cfg.Upstream)
}

// checkinTrae 单号 Trae 签到：Checkin（status → 未签到则 claim）+ QueryCredits 刷新额度。
// 响应字段与 WorkBuddy 单号签到保持一致（checkin_done / checkin_message / credits / credits_total）。
func (p *Panel) checkinTrae(w http.ResponseWriter, a *auth.Auth) {
	ctx := context.Background()
	client := p.clientFor(auth.ProviderTrae)
	if client == nil {
		writeErr(w, http.StatusServiceUnavailable, "Trae provider 未接入或未启用")
		return
	}
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
