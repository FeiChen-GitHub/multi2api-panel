// sticky.go 面板「粘性会话」管理：只读枚举 + 批量解绑。
//
// 依赖 main 注入的两个闭包（StickySessions / UnbindStickySessions），panel 自身
// 不 import session 包——粘性路由是进程内单例，经闭包接线保持分层。
// 未注入（粘性路由关闭 / 纯测试面板）时两个接口返回 501，前端隐藏入口。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// StickySession 一条粘性会话绑定（面板展示用）。
// SessionKey 原样透出（含 provider 前缀，如 "trae|<conversation_id>" 或
// "trae|d-<hex>"），由 main 装配时经 pool.StatusByKey 补全账号展示字段。
type StickySession struct {
	// SessionKey 会话键（provider|sessKey，含 provider 前缀；运维可识别）。
	SessionKey string `json:"session_key"`
	// AccountKey 绑定的账号主键（UUIDv4；路由参数用主键，跨 provider 唯一）。
	AccountKey string `json:"account_key"`
	// UID 上游账号号（展示；可能跨 provider 重复，不作路由参数）。
	UID string `json:"uid,omitempty"`
	// Nickname 账号昵称（展示）。
	Nickname string `json:"nickname,omitempty"`
	// Provider 账号所属上游（"workbuddy" / "trae"）。
	Provider string `json:"provider,omitempty"`
	// Realm 账号域（cn / global）。
	Realm string `json:"realm,omitempty"`
	// LastActive 最近活跃时刻（RFC3339；展示「多久前」）。
	LastActive time.Time `json:"last_active,omitempty"`
	// 以下聚合自请求归档（reqlog.Event.SessionKey 对齐）：同一会话最近请求的
	// 标题/模型/结果/来源/token。归档关闭或无历史时为空/零。
	Title      string `json:"title,omitempty"`
	// LastQuestion 该会话缓存的最后一条 user 消息（进程内缓存、不落盘）。
	// 空 = 无缓存（粘性路由未启用 / 尚无新请求 / 缓存 TTL 过期）。
	LastQuestion string `json:"last_question,omitempty"`
	Model      string `json:"model,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Status     int    `json:"status,omitempty"`
	ClientIP   string `json:"client_ip,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
	TotalTokens int64 `json:"total_tokens,omitempty"`
}

// stickySessions 返回全部粘性会话明细（按会话键排序；账号展示字段由 main 注入的
// StickySessions 闭包统一补全，panel 不做账号反查）。未注入时返回 nil,false（501）。
func (p *Panel) stickySessions() ([]StickySession, bool) {
	if p.cfg.StickySessions == nil {
		return nil, false
	}
	return p.cfg.StickySessions(), true
}

// stickyList GET /panel/api/sticky
// 返回全部粘性会话绑定（供「粘性会话」视图渲染）。
func (p *Panel) stickyList(w http.ResponseWriter, r *http.Request) {
	rows, ok := p.stickySessions()
	if !ok {
		writeErr(w, http.StatusNotImplemented, "sticky_not_available")
		return
	}
	p.aggregateStickyFromArchive(rows)
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "count": len(rows)})
}

// aggregateStickyFromArchive 按 reqlog 归档里的 SessionKey 聚合每个粘性会话的
// 标题/模型/结果/来源/累计 token。归档关闭或无历史时保持空（前端 "-" 兜底）。
// 只读最近 1000 条请求；会话最后活跃时间以 session.Bindings 的 LastActive 为准。
func (p *Panel) aggregateStickyFromArchive(rows []StickySession) {
	if p.cfg.RequestLog == nil || len(rows) == 0 {
		return
	}
	events, err := p.cfg.RequestLog.ReadArchive(1000, reqlog.Filter{})
	if err != nil || len(events) == 0 {
		return
	}
	// 每会话：最近一条（事件倒序，首见即最新）+ token 累加
	type agg struct {
		title, model, outcome, ip, ua string
		status                        int
		total                         int64
		seen                          bool
	}
	by := make(map[string]*agg, len(rows))
	for i := range rows {
		by[rows[i].SessionKey] = &agg{}
	}
	for _, e := range events {
		if e.SessionKey == "" {
			continue
		}
		a, ok := by[e.SessionKey]
		if !ok {
			continue
		}
		a.total += e.TotalTokens
		if !a.seen {
			a.seen = true
			a.title, a.model, a.outcome, a.ip, a.ua = e.Title, e.Model, e.Outcome, e.ClientIP, e.UserAgent
			a.status = e.Status
		}
	}
	for i := range rows {
		a := by[rows[i].SessionKey]
		if a == nil || !a.seen {
			continue
		}
		rows[i].Title = a.title
		rows[i].Model = a.model
		rows[i].Outcome = a.outcome
		rows[i].Status = a.status
		rows[i].ClientIP = a.ip
		rows[i].UserAgent = a.ua
		rows[i].TotalTokens = a.total
	}
}

// stickyUnbind POST /panel/api/sticky/unbind
// 批量解绑指定会话键。body: {"keys": ["trae|<sess>", ...]}
// 只解绑会话与账号的粘性绑定，不改动账号池；返回实际解绑条数。
func (p *Panel) stickyUnbind(w http.ResponseWriter, r *http.Request) {
	if p.cfg.UnbindStickySessions == nil {
		writeErr(w, http.StatusNotImplemented, "sticky_not_available")
		return
	}
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request: "+err.Error())
		return
	}
	// 防御：空键/超长/非可打印不提交（会话键是内部值，正常不会出现这些形态）。
	keys := make([]string, 0, len(req.Keys))
	seen := map[string]bool{}
	for _, k := range req.Keys {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > 512 || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound": 0})
		return
	}
	n, err := p.cfg.UnbindStickySessions(keys)
	if err != nil {
		log.Printf("panel: sticky unbind failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "unbind_failed")
		return
	}
	log.Printf("panel: sticky unbind keys=%d unbound=%d", len(keys), n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound": n})
}





