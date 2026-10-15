// trae_chat.go Trae provider 的响应处理：SOLO 原生流 → OpenAI 格式，并把流内业务
// 错误落到池上（冷却/禁用）。与 WorkBuddy 路径分开，避免两套 SSE 语义互相污染。
package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// classifyFor 按 provider 选择错误分类器：Trae 有自己的状态码/流内码语义
// （1005/4008 权益不足、4011 限流、401 session 失效），不能套 WorkBuddy 的 Classify。
func (h *Handler) classifyFor(provider auth.Provider, status int, body string) upstream.ErrKind {
	if provider == auth.ProviderTrae {
		return upstream.TraeClassify(status, body)
	}
	return upstream.Classify(status, body)
}

// classifyForChannel 按 provider + 通道选择错误分类器。
// Trae 的 Work 通道有自己的失败形态（work_credits 不足、4001/4000105 原生任务错误，
// DESIGN §15.5 D8），不能套 SOLO 的 TraeClassify；非 Trae / 非 Work 一律按
// classifyFor 原口径（零回归）。
func (h *Handler) classifyForChannel(provider auth.Provider, ch upstream.Channel, status int, body string) upstream.ErrKind {
	if provider == auth.ProviderTrae && ch == upstream.ChannelWork {
		return upstream.TraeWorkClassify(status, body)
	}
	return h.classifyFor(provider, status, body)
}

// applyTraeStreamError 把 SOLO 流内业务错误落到池上，冷却口径与 WorkBuddy 对齐：
// 权益/配额不足 → 硬冷却到次日 04:00；限流 → 软冷却；session 失效 → 禁用；
// 其余 → 记错误计数（喂熔断/连败）。
func (h *Handler) applyTraeStreamError(acct *auth.Auth, model string, se *upstream.TraeStreamError) {
	switch se.Kind() {
	case upstream.ErrHardCredit:
		h.cfg.Pool.CooldownUntilTomorrow4AM(acct.UID, "trae 权益/配额不足: "+se.Error())
	case upstream.ErrSoftRate:
		h.cfg.Pool.CooldownSoftRate(acct.UID, h.softCooldown(), time.Time{}, "trae 限流: "+se.Error())
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(acct.UID, "trae session dead")
	default:
		h.cfg.Pool.NoteError(acct.UID)
	}
	log.Printf("WARN: [server] trae stream acct=%s model=%s kind=%v: %v",
		logfmt.Label(acct.UID, acct.Nickname), model, se.Kind(), se)
}

// applyTraeWorkStreamError 把 Work 通道的流内错误落到池上（DESIGN §6.6 + D8）。
//
// 与 SOLO 版本的关键差异：权益/配额不足**只冷 Work 通道**（workUntil），不动账号级
// until —— SOLO 出口没撞限额，不该被连坐。其余形态（session 失效 / 限流 / 未知）
// 两条通道共用凭证与身份，按账号级处置。
func (h *Handler) applyTraeWorkStreamError(acct *auth.Auth, model string, se *upstream.TraeStreamError) {
	switch se.Kind() {
	case upstream.ErrHardCredit:
		h.cfg.Pool.CooldownWorkUntilTomorrow4AM(acct.UID, pool.TraeWorkCreditReason())
	case upstream.ErrSoftRate:
		h.cfg.Pool.CooldownSoftRate(acct.UID, h.softCooldown(), time.Time{}, "trae work 限流: "+se.Error())
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(acct.UID, "trae session dead")
	default:
		h.cfg.Pool.NoteError(acct.UID)
	}
	log.Printf("WARN: [server] trae work stream acct=%s model=%s kind=%v: %v",
		logfmt.Label(acct.UID, acct.Nickname), model, se.Kind(), se)
}

// serveTraeSuccess 处理 Trae 的一次 200 响应：
//   - 流式：原生流 → OpenAI SSE；流内 event:error 经回调冷却账号；
//   - 非流式：聚合为 chat.completion 后一次写出。
//
// 成功判定与粘性绑定口径与 WorkBuddy 一致：整段读完且无流内错误才算成功。
// ch 决定用哪套解析器：Work 的事件是 payload 包裹的 plan_item/output/done 且
// output 是累积全文，SOLO 是扁平的 response/reasoning_content —— 两套形状不能
// 互相套用（套错的一侧会输出空内容）。
func (h *Handler) serveTraeSuccess(w http.ResponseWriter, r *http.Request, acct *auth.Auth, rc io.ReadCloser, model string, stream bool, st *chatStat, sessKey string, ch upstream.Channel) {
	defer rc.Close()

	work := ch == upstream.ChannelWork
	onErr := func(se *upstream.TraeStreamError) { h.applyTraeStreamError(acct, model, se) }
	onWorkErr := func(se *upstream.TraeStreamError) { h.applyTraeWorkStreamError(acct, model, se) }
	handleErr := onErr
	if work {
		handleErr = onWorkErr
	}

	if stream {
		st.status = http.StatusOK
		var streamErr *upstream.TraeStreamError
		var err error
		if work {
			err = upstream.TraeStreamWorkToOpenAI(w, rc, model, func(se *upstream.TraeStreamError) {
				streamErr = se
				onWorkErr(se)
			})
		} else {
			err = upstream.TraeStreamToOpenAI(w, rc, model, func(se *upstream.TraeStreamError) {
				streamErr = se
				onErr(se)
			})
		}
		if err != nil {
			// 客户端写失败（断连）：上游帧无恙，账号照常记成功（与 WorkBuddy 同口径）。
			st.outcome = reqlog.OutcomeInterrupted
			h.cfg.Pool.NoteSuccess(acct.UID)
			return
		}
		if streamErr != nil {
			st.status = http.StatusServiceUnavailable
			st.outcome = reqlog.OutcomeStreamError
			return
		}
		h.markTraeSuccess(acct, model, st, sessKey)
		if work {
			h.probeTraeWorkCredits(acct)
		}
		return
	}

	var resp map[string]any
	var err error
	if work {
		resp, err = upstream.TraeAggregateWork(rc, model)
	} else {
		resp, err = upstream.TraeAggregate(rc, model)
	}
	if err != nil {
		var se *upstream.TraeStreamError
		if errors.As(err, &se) {
			handleErr(se)
			st.status = http.StatusServiceUnavailable
			st.outcome = reqlog.OutcomeStreamError
			writeOpenAIError(w, http.StatusServiceUnavailable, "trae_upstream_error", se.Error())
			return
		}
		st.status = http.StatusBadGateway
		st.outcome = reqlog.OutcomeStreamError
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	h.markTraeSuccess(acct, model, st, sessKey)
	if work {
		h.probeTraeWorkCredits(acct)
	}
	writeJSON(w, http.StatusOK, resp)
}

// probeTraeWorkCredits 异步刷新账号的 work_credits（DESIGN §6.6 ProbeCredits）。
// Work 通道没有每请求回传余额的机制，只能探测；余额归零即把该通道冷却到次日，
// 免得后续请求继续往没有额度的出口上撞。探测失败只记日志——**不写 0**，否则会把
// 健康号误判成余额耗尽而误冷却。
func (h *Handler) probeTraeWorkCredits(acct *auth.Auth) {
	if h.cfg.TraeWorkDisabled {
		return // Work 通道被配置关闭：不做无谓的探测
	}
	client, ok := h.clientFor(auth.ProviderTrae).(*upstream.TraeClient)
	if !ok || client == nil {
		return
	}
	go func(a *auth.Auth) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap, err := client.ProbeCredits(ctx, a)
		if err != nil {
			log.Printf("WARN: [server] trae work_credits probe acct=%s: %v", logfmt.Label(a.UID, a.Nickname), err)
			return
		}
		h.cfg.Pool.SetWorkCredits(a.UID, snap.WorkCredits)
		if snap.WorkCredits <= 0 {
			h.cfg.Pool.CooldownWorkUntilTomorrow4AM(a.UID, pool.TraeWorkCreditReason())
		}
	}(acct)
}

// markTraeSuccess 记成功 + 清模型负缓存 + 粘性跟随（与 WorkBuddy 成功路径同口径）。
func (h *Handler) markTraeSuccess(acct *auth.Auth, model string, st *chatStat, sessKey string) {
	st.status = http.StatusOK
	st.outcome = reqlog.OutcomeSuccess
	h.cfg.Pool.NoteSuccess(acct.UID)
	h.cfg.Pool.BlockModelClear(acct.UID, model)
	if sessKey != "" && h.cfg.Session != nil {
		h.cfg.Session.Bind(sessKey, acct.Key())
	}
}
