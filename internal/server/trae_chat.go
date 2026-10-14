// trae_chat.go Trae provider 的响应处理：SOLO 原生流 → OpenAI 格式，并把流内业务
// 错误落到池上（冷却/禁用）。与 WorkBuddy 路径分开，避免两套 SSE 语义互相污染。
package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
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

// serveTraeSuccess 处理 Trae 的一次 200 响应：
//   - 流式：SOLO 原生流 → OpenAI SSE；流内 event:error 经回调冷却账号；
//   - 非流式：聚合为 chat.completion 后一次写出。
//
// 成功判定与粘性绑定口径与 WorkBuddy 一致：整段读完且无流内错误才算成功。
func (h *Handler) serveTraeSuccess(w http.ResponseWriter, r *http.Request, acct *auth.Auth, rc io.ReadCloser, model string, stream bool, st *chatStat, sessKey string) {
	defer rc.Close()

	if stream {
		st.status = http.StatusOK
		var streamErr *upstream.TraeStreamError
		err := upstream.TraeStreamToOpenAI(w, rc, model, func(se *upstream.TraeStreamError) {
			streamErr = se
			h.applyTraeStreamError(acct, model, se)
		})
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
		return
	}

	resp, err := upstream.TraeAggregate(rc, model)
	if err != nil {
		var se *upstream.TraeStreamError
		if errors.As(err, &se) {
			h.applyTraeStreamError(acct, model, se)
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
	writeJSON(w, http.StatusOK, resp)
}

// markTraeSuccess 记成功 + 清模型负缓存 + 粘性跟随（与 WorkBuddy 成功路径同口径）。
func (h *Handler) markTraeSuccess(acct *auth.Auth, model string, st *chatStat, sessKey string) {
	st.status = http.StatusOK
	st.outcome = reqlog.OutcomeSuccess
	h.cfg.Pool.NoteSuccess(acct.UID)
	h.cfg.Pool.BlockModelClear(acct.UID, model)
	if sessKey != "" && h.cfg.Session != nil {
		h.cfg.Session.Bind(sessKey, acct.UID)
	}
}
