// trae_login.go Trae 网页 OAuth 登录闭环（DESIGN §16 缺口 2）。
//
//	POST /panel/api/login/trae/start  → 生成登录 URL + pending 态（machine/device id 对）
//	GET  /panel/api/login/trae/result → 面板轮询（pending / success / failed）
//	GET  /authorize                   → 浏览器回调落点（**不套 withAuth**）
//
// 与 WorkBuddy 设备授权（login.go）的两点关键差异：
//  1. Trae 是「浏览器登录 → 302 回本地回调 URL」，不是服务端轮询上游 state，
//     所以回调落点必须是一个**免鉴权**的 GET 路由（浏览器跳转带不上 Bearer）；
//  2. 回调只回传 login_trace_id（不回 machine_id/device_id），因此 pending 靠
//     login_trace_id 反查，把登录时生成的那一对 id 找回来写进凭证文件——
//     否则签到的 X-Device-Id 与登录态不一致。
//
// 回调端口策略：Trae 登录页要求 auth_callback_url 指向固定端口（默认 18080），
// main 另起一个 http.Server 复用同一 Handler（/authorize 在本 mux 已注册）。
// pending 只在内存（重启丢失），符合「登录态瞬时」语义。
package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TraeDefaultCallbackPort 默认回调监听端口（与 trae2api 同策略）。
const TraeDefaultCallbackPort = "18080"

// traeLoginTTL pending 登录态的有效期：超时的 pending 直接回收，防止「点了登录
// 就走开」的会话永久滞留。
const traeLoginTTL = 10 * time.Minute

// traeLoginState pending 登录状态。
type traeLoginState string

const (
	traeLoginPending  traeLoginState = "pending"  // 等浏览器回调
	traeLoginSuccess  traeLoginState = "success"  // 回调捕获 + 落盘成功
	traeLoginFailed   traeLoginState = "failed"   // 换 token / 落盘失败
	traeLoginCanceled traeLoginState = "canceled" // 用户取消
)

// traePendingLogin 单次 Trae 登录的临时上下文。
type traePendingLogin struct {
	state       traeLoginState
	machineID   string // hex32
	deviceID    string // 16 位数字（签到口径）
	callbackURL string // auth_callback_url（含端口）
	createdAt   time.Time

	uid      string // success 时填
	nickname string
	errMsg   string // failed 时填
}

// TraeLoginURL 构造 Trae 网页登录 URL（参数集对照 trae2api BuildLoginURL）。
// machineID/deviceID 必须与落盘凭证共用同一对：登录态与凭证不一致会导致
// 签到被「该设备已签到」拦截。
func TraeLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", upstream.TraeClientID)
	v.Set("redirect", "0")
	return upstream.TraeConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(traeTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(traeLoginAppVersion) +
		"&x_app_type=stable"
}

// traeLoginAppVersion 登录页上报的 IDE 版本。与 trae2api 的 loginAppVersion 同语义
// （upstream 侧常量未导出，这里显式手抄并标注来源，避免改动上游既有命名）。
const traeLoginAppVersion = "0.1.52"

// traeTraceIDHexLen login_trace_id 的 hex 长度（16 hex 字符）。
const traeTraceIDHexLen = 16

// traeTraceID 由 machineID+deviceID 派生稳定的 login_trace_id（hex16）。
//
// 回调不回传 machine_id/device_id，只回传 login_trace_id；用输入拼接的尾部 16
// 字符即可稳定反查回登录时生成的那一对 id（两者均为定长，尾部唯一性足够）。
func traeTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= traeTraceIDHexLen {
		return h[len(h)-traeTraceIDHexLen:]
	}
	return strings.Repeat("0", traeTraceIDHexLen-len(h)) + h
}

// TraeCallbackInfo 回调链接解析结果（原始凭证，仅服务端内部使用）。
type TraeCallbackInfo struct {
	RefreshToken string // 优先 query.refreshToken，缺省回退 userJwt.RefreshToken
	AccessToken  string // 无 refreshToken 时回退 userJwt.Token（兜底）
	UID          string // userInfo.UserID
	Nickname     string // userInfo.ScreenName
	EnterpriseID string // userInfo.TenantID（回调字段名是 TenantID）
	ExpiresAt    int64  // Unix 秒（仅 userJwt 兜底路径有值）
}

// TraeParseCallback 解析 Trae 登录回调链接。
//
//	回调形如 http://127.0.0.1:18080/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//
// userInfo / userJwt 是 URL 编码的 JSON，容错再解一层 percent-encoding
// （照 trae2api 的 parse_json_param；浏览器与 IDE 的编码层数不总是一致）。
// 只做解析，换 token 由 upstream 的 RefreshToken 完成。
func TraeParseCallback(rawURL string) (*TraeCallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("empty callback url")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &TraeCallbackInfo{RefreshToken: q.Get("refreshToken")}

	userInfo := traeJSONParam(q.Get("userInfo"))
	info.UID = traeJSONString(userInfo, "UserID")
	info.Nickname = traeJSONString(userInfo, "ScreenName")
	info.EnterpriseID = traeJSONString(userInfo, "TenantID")

	userJwt := traeJSONParam(q.Get("userJwt"))
	jwtToken := traeJSONString(userJwt, "Token")
	if info.RefreshToken == "" {
		info.RefreshToken = traeJSONString(userJwt, "RefreshToken")
	}
	if info.RefreshToken == "" {
		// 兜底：没有 refreshToken 时直接用 userJwt.Token 当 access token。
		info.AccessToken = jwtToken
		if jwtToken == "" {
			return nil, errors.New("callback missing refreshToken and userJwt.Token")
		}
		if exp := traeJSONInt(userJwt, "TokenExpireAt"); exp > 0 {
			info.ExpiresAt = traeNormalizeExpire(exp)
		}
	}
	return info, nil
}

// traeJSONParam 解回调里 URL 编码的 JSON 参数（容错多解一层 unquote）。
func traeJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func traeJSONString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return fmt.Sprintf("%v", v)
}

func traeJSONInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// traeNormalizeExpire TokenExpireAt 毫秒 → Unix 秒（>1e12 视为毫秒）。
func traeNormalizeExpire(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// loginTraeStart POST /panel/api/login/trae/start：生成登录 URL + pending 态。
// body 可选 {callback_port: "18080"}；默认 127.0.0.1:18080/authorize。
func (p *Panel) loginTraeStart(w http.ResponseWriter, r *http.Request) {
	if p.traeClient() == nil {
		writeErr(w, http.StatusServiceUnavailable, "Trae provider 未接入或未启用")
		return
	}
	port := TraeDefaultCallbackPort
	if r.Body != nil {
		var reqBody struct {
			CallbackPort string `json:"callback_port"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if strings.TrimSpace(reqBody.CallbackPort) != "" {
				port = strings.TrimSpace(reqBody.CallbackPort)
			}
		}
	}
	callbackURL := "http://127.0.0.1:" + port + "/authorize"

	machineID, err := traeRandomHex(16) // hex32
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand failed: "+err.Error())
		return
	}
	// deviceID 用「签到兼容」的 16 位纯数字：与真实注册设备号同形，保证该账号
	// 随后的每日签到能成功且与其他账号互异（auth.NewCheckinDeviceID）。
	deviceID, err := auth.NewCheckinDeviceID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand failed: "+err.Error())
		return
	}
	pendingID, err := traeRandomHex(8) // hex16
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "rand failed: "+err.Error())
		return
	}
	pl := &traePendingLogin{
		state:       traeLoginPending,
		machineID:   machineID,
		deviceID:    deviceID,
		callbackURL: callbackURL,
		createdAt:   time.Now(),
	}
	p.traeLoginMu.Lock()
	for id, old := range p.traeLogins {
		if time.Since(old.createdAt) > traeLoginTTL {
			delete(p.traeLogins, id)
		}
	}
	p.traeLogins[pendingID] = pl
	p.traeLoginMu.Unlock()

	log.Printf("panel: 发起 Trae 网页登录（pending=%s，回调 %s）", pendingID, callbackURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"pending_id":   pendingID,
		"login_url":    TraeLoginURL(machineID, deviceID, callbackURL),
		"callback_url": callbackURL,
	})
}

// loginTraeResult GET /panel/api/login/trae/result?pending_id=...
func (p *Panel) loginTraeResult(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("pending_id")
	pl, ok := p.traePending(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "pending login not found（已过期或无效，请重新发起）")
		return
	}
	resp := map[string]any{"pending_id": id, "state": string(pl.state)}
	if pl.state == traeLoginSuccess {
		resp["uid"] = pl.uid
		resp["nickname"] = pl.nickname
	} else if pl.state == traeLoginFailed {
		resp["error"] = pl.errMsg
	}
	writeJSON(w, http.StatusOK, resp)
}

// loginTraeCancel POST /panel/api/login/trae/cancel（body {pending_id} 或 query）。
func (p *Panel) loginTraeCancel(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("pending_id")
	if id == "" && r.Body != nil {
		var reqBody struct {
			PendingID string `json:"pending_id"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&reqBody)
		id = reqBody.PendingID
	}
	p.traeLoginMu.Lock()
	_, ok := p.traeLogins[id]
	delete(p.traeLogins, id)
	p.traeLoginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"pending_id": id, "canceled": ok})
}

// TraeAuthorizeHandler 暴露回调 handler 供主路由挂载（见 traeAuthorize）。
//
// 为什么不由 panel 自挂：panel 的 mux 只处理 /panel/ 前缀（外层按前缀转发），
// 而 Trae 的回调 URL 是固定端口下的 /authorize，必须挂在根 mux 上才能被
// 18080 那个 server 命中。同样的导出方式也便于测试直接打这个 handler。
func (p *Panel) TraeAuthorizeHandler() http.HandlerFunc { return p.traeAuthorize }

// traeAuthorize GET /authorize：Trae 登录回调落点。
//
// 浏览器跳转，不能套 withAuth（带不上 Bearer）。捕获 query → 解析 → ExchangeToken
// → GetUserInfo 补全 → 落盘 trae-<uid>.json → 热加载进池 → 标记 pending 成功。
// 响应是给人看的 HTML 页（不是 JSON），让用户知道可以关窗口回面板。
func (p *Panel) traeAuthorize(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.String()
	if !strings.HasPrefix(rawURL, "http") {
		// 反向代理/相对形式到达时补全成绝对 URL 再解析。
		rawURL = "http://127.0.0.1" + rawURL
	}
	info, err := TraeParseCallback(rawURL)
	if err != nil {
		p.traeRender(w, http.StatusBadRequest, "登录回调解析失败", err.Error())
		return
	}
	// 回调不回传 machine_id/device_id（只回 login_trace_id）→ 用它反查 pending，
	// 找回登录时生成的那一对 id；查不到就沿用回调里带的（可能为空）。
	machineID := r.URL.Query().Get("machine_id")
	deviceID := r.URL.Query().Get("device_id")
	traceID := r.URL.Query().Get("login_trace_id")
	if traceID == "" {
		traceID = r.URL.Query().Get("loginTraceID")
	}
	if (machineID == "" || deviceID == "") && traceID != "" {
		if pl, ok := p.traePendingByTrace(traceID); ok {
			machineID, deviceID = pl.machineID, pl.deviceID
		}
	}

	a := &auth.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		UID:          info.UID,
		Nickname:     info.Nickname,
		EnterpriseID: info.EnterpriseID,
		Domain:       "trae.cn",
		ApiHost:      p.traeOAuthHost(),
		MachineID:    machineID,
		DeviceID:     deviceID,
		ExpiresAt:    info.ExpiresAt,
	}
	auth.SetProvider(a, auth.ProviderTrae)

	client := p.traeClient()
	if client == nil {
		p.markTraePending(traceID, traeLoginFailed, "", "", "Trae provider 未接入或未启用")
		p.traeRender(w, http.StatusServiceUnavailable, "登录失败", "Trae provider 未接入或未启用")
		return
	}
	// 有 refreshToken → ExchangeToken 换 access token（refreshToken 同时轮换）。
	if a.RefreshToken != "" {
		if err := client.RefreshToken(r.Context(), a); err != nil {
			p.markTraePending(traceID, traeLoginFailed, "", "", "ExchangeToken 失败: "+err.Error())
			p.traeRender(w, http.StatusBadGateway, "登录失败", "ExchangeToken 失败："+err.Error())
			return
		}
	}
	// GetUserInfo 补全 uid/nickname/enterprise（回调 userInfo 可能缺字段）。
	if uid, nick, ent, err := p.traeGetUserInfo(r.Context(), a); err == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	switch {
	case a.UID == "":
		err := errors.New("无法从回调或 GetUserInfo 确定 uid")
		p.markTraePending(traceID, traeLoginFailed, "", "", err.Error())
		p.traeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	case a.AccessTokenValue() == "":
		err := errors.New("换 token 后仍无 access token")
		p.markTraePending(traceID, traeLoginFailed, a.UID, a.Nickname, err.Error())
		p.traeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	// UID 来自上游，拼文件名前必须校验（同 importTrae 的防路径穿越口径）。
	if !validImportUID(a.UID) {
		err := errors.New("uid 含非法字符，拒绝落盘")
		p.markTraePending(traceID, traeLoginFailed, "", "", err.Error())
		p.traeRender(w, http.StatusBadRequest, "登录失败", err.Error())
		return
	}
	if _, _, err := a.EnsureKey(); err != nil {
		p.markTraePending(traceID, traeLoginFailed, a.UID, a.Nickname, "gen account key: "+err.Error())
		p.traeRender(w, http.StatusInternalServerError, "登录失败", "生成账号 key 失败："+err.Error())
		return
	}
	// phase 1 只支持 cn（与 importTrae 同口径）。
	if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
		p.markTraePending(traceID, traeLoginFailed, a.UID, a.Nickname, "set realm: "+err.Error())
		p.traeRender(w, http.StatusInternalServerError, "登录失败", "写入 realm 失败："+err.Error())
		return
	}
	// 签到 deviceId 兜底：回调没带 device_id 时补一个（每号互异，16 位数字）。
	if _, err := a.EnsureCheckinDeviceID(); err != nil {
		p.markTraePending(traceID, traeLoginFailed, a.UID, a.Nickname, "gen device id: "+err.Error())
		p.traeRender(w, http.StatusInternalServerError, "登录失败", "生成 deviceId 失败："+err.Error())
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		p.markTraePending(traceID, traeLoginFailed, a.UID, a.Nickname, "mkdir auth dir: "+err.Error())
		p.traeRender(w, http.StatusInternalServerError, "落盘失败", err.Error())
		return
	}
	a.FilePath = filepath.Join(p.cfg.AuthDir, "trae-"+a.UID+".json")
	if err := a.SaveAtomic(); err != nil {
		p.markTraePending(traceID, traeLoginFailed, a.UID, a.Nickname, "save auth: "+err.Error())
		p.traeRender(w, http.StatusInternalServerError, "落盘失败", err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID) // 全新登录 = 人工恢复口径：清掉旧号遗留的禁用/冷却
	p.markTraePending(traceID, traeLoginSuccess, a.UID, a.Nickname, "")
	log.Printf("panel: Trae 网页登录完成 uid=%s nickname=%q（已落盘并热加载）", a.UID, a.Nickname)
	p.traeRender(w, http.StatusOK, "登录成功",
		"账号 "+a.UID+"（"+a.Nickname+"）已添加，可关闭此窗口返回面板。")
}

// traeClient 返回 Trae 上游客户端（未接入/未启用返回 nil）。
// 只有 *upstream.TraeClient 才有 GetUserInfo，故做一次类型断言。
func (p *Panel) traeClient() upstream.ChatClient {
	return p.clientFor(auth.ProviderTrae)
}

// traeOAuthHost 登录用的 OAuth host：配置覆盖优先，否则内置默认。
// 绝不从回调 query 里读 host——那是浏览器可控输入，会被用来把 refreshToken
// 发到任意域（token 外泄）。host 只能来自配置。
func (p *Panel) traeOAuthHost() string {
	if p.cfg.TraeApiHost != "" {
		return p.cfg.TraeApiHost
	}
	return upstream.TraeOAuthHost
}

// traeGetUserInfo 经 TraeClient 补全账号信息；客户端不是 TraeClient（或为 nil）
// 时返回错误，由调用方按「回调里已有的值」继续。
func (p *Panel) traeGetUserInfo(ctx context.Context, a *auth.Auth) (uid, nick, ent string, err error) {
	tc, ok := p.traeClient().(*upstream.TraeClient)
	if !ok {
		return "", "", "", errors.New("trae client not available")
	}
	return tc.GetUserInfo(ctx, a)
}

// traePending 取 pending 态（不存在或已过期返回 false）。
func (p *Panel) traePending(id string) (*traePendingLogin, bool) {
	if id == "" {
		return nil, false
	}
	p.traeLoginMu.Lock()
	defer p.traeLoginMu.Unlock()
	pl, ok := p.traeLogins[id]
	if !ok {
		return nil, false
	}
	if time.Since(pl.createdAt) > traeLoginTTL {
		delete(p.traeLogins, id)
		return nil, false
	}
	return pl, true
}

// traePendingByTrace 用 login_trace_id 反查 pending。
func (p *Panel) traePendingByTrace(traceID string) (*traePendingLogin, bool) {
	if traceID == "" {
		return nil, false
	}
	p.traeLoginMu.Lock()
	defer p.traeLoginMu.Unlock()
	for _, pl := range p.traeLogins {
		if traeTraceID(pl.machineID, pl.deviceID) == traceID {
			return pl, true
		}
	}
	return nil, false
}

// markTraePending 用 login_trace_id 反查 pending 并标记终态（success/failed）。
func (p *Panel) markTraePending(traceID string, state traeLoginState, uid, nick, errMsg string) {
	if traceID == "" {
		return
	}
	p.traeLoginMu.Lock()
	defer p.traeLoginMu.Unlock()
	for _, pl := range p.traeLogins {
		if traeTraceID(pl.machineID, pl.deviceID) != traceID {
			continue
		}
		pl.state = state
		if uid != "" {
			pl.uid = uid
		}
		if nick != "" {
			pl.nickname = nick
		}
		if errMsg != "" {
			pl.errMsg = errMsg
		}
		return
	}
}

// traeRender 渲染一个极简结果页（回调是浏览器跳转，必须给人看而不是给 JSON）。
func (p *Panel) traeRender(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>`+
		`<div style="font-family:system-ui,sans-serif;max-width:520px;margin:80px auto;line-height:1.7">`+
		`<h2 style="margin:0 0 12px">%s</h2><p style="color:#444;margin:0">%s</p></div>`,
		htmlEscape(title), htmlEscape(title), htmlEscape(detail))
}

// htmlEscape 转义结果页里来自上游的文本（uid/昵称/错误信息都不可信）。
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return strings.ReplaceAll(s, "'", "&#39;")
}

// traeRandomHex 生成 n 字节随机 hex（2n hex 字符）。
func traeRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
