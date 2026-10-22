// trae_client.go TRAE SOLO 上游客户端（ChatClient 实现）。
//
// 端点/头/常量对照 trae2api（internal/upstream/constants.go、headers.go、client.go，已核实）：
//   - 对话：POST {AgentHost}/api/agent/v3/llm_utils_chat（请求体经 TraePrepareBody 改写）
//   - 模型：POST {AgentHost}/api/ide/v1/get_detail_param
//   - 刷新：POST {OAuthHost}/cloudide/api/v3/trae/oauth/ExchangeToken
//   - 签到：POST {UgHost}/trae/api/v2/ug/checkin_credits/{status,claim}
//   - 额度：POST {UgHost}/trae/api/v2/pay/ide_user_ent_usage
//
// 与 trae2api 一致地使用两个 http.Client：短请求有总超时，SSE 无总超时（靠
// ResponseHeaderTimeout 兜底），两者共享同一 Transport。phase 1 只支持 CN。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	TraeAgentHost = "https://trae-api-cn.mchost.guru"
	TraeUgHost    = "https://api.trae.cn"
	TraeOAuthHost = "https://api.trae.com.cn"
	// TraeConsoleHost 网页端控制台域名：登录页与 OAuth 跳转都挂在它下面
	// （对照 trae2api constants.go 的 ConsoleHost）。登录 URL 只在这里构造，
	// 网关侧不参与回调解析之外的人机交互。
	TraeConsoleHost = "https://www.trae.cn"
	TraeClientID    = "en1oxy7wnw8j9n"
	// TraeAppID SOLO 应用的 X-App-Id（与 trae2api constants.AppID、本仓库
	// TraeWorkAppIDChat 一致）。此前误写成 ...-9e5a-...（正确为 ...-9ca5-...），
	// 后端按 (app_id, app_version_code) 查不到 app 配置记录，SOLO 对话恒报
	// code=2001 "failed to get app config: record not found"。
	TraeAppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"

	traeIdeVersion     = "0.1.52"
	traeIdeVersionCode = "20260811"
	// 模型发现的 get_detail_param 用 3.4.4 头：3.4.4 的 solo_agent 返回 44 条
	// 真实可用的 agent 模型（glm-5.x、DeepSeek-V4、minimax-m3、kimi-k2 等）。
	// 注意对话端点 llm_utils_chat 必须仍用 0.1.52 头——实测 3.4.4 头打对话
	// 会返回 SSE code:4001 "param is invalid"（app_id+app_version_code 查不到配置），
	// 故模型发现与对话走两套版本头。
	traeModelIdeVersion     = "3.4.4"
	traeModelIdeVersionCode = "20260212"
	traeOSVersion      = "macOS 15.7.4"
	traeDeviceBrand    = "Apple"

	TraeEpChat          = "/api/agent/v3/llm_utils_chat"
	TraeEpModels        = "/api/ide/v1/get_detail_param"
	TraeEpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	TraeEpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	TraeEpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	TraeEpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	TraeEpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// TraeClient TRAE SOLO 上游客户端。Host 字段可覆盖，便于测试与后续多域名。
type TraeClient struct {
	// HTTP 短 JSON 请求（刷新/签到/额度），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP SSE 对话：不设总超时，避免长流被截断；靠 Transport 的
	// ResponseHeaderTimeout 兜底「上游一直不返回首字节」。nil 时回落 HTTP。
	StreamHTTP *http.Client

	AgentHost string
	UgHost    string
	OAuthHost string
	ClientID  string
}

// NewTraeClient 生产默认值（连接池复用，同 trae2api.New）。
func NewTraeClient() *TraeClient {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &TraeClient{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		AgentHost:  TraeAgentHost,
		UgHost:     TraeUgHost,
		OAuthHost:  TraeOAuthHost,
		ClientID:   TraeClientID,
	}
}

var _ ChatClient = (*TraeClient)(nil)

// traeSOLOHeaders 设置 llm_utils_chat 对话端点所需头（实测必须）。
// 对话必须用 0.1.52 头：3.4.4 头打 llm_utils_chat 会被上游以配置不存在为由拒。
func traeSOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	traeSOLOHeadersWith(req, a, stream, traeIdeVersion, traeIdeVersionCode)
}

// traeModelHeaders 设置 get_detail_param 模型发现端点所需头，用 3.4.4
// 版本（solo_agent 返回 44 条真实 agent 模型）。与对话头分离以避免影响对话。
func traeModelHeaders(req *http.Request, a *auth.Auth) {
	traeSOLOHeadersWith(req, a, false, traeModelIdeVersion, traeModelIdeVersionCode)
}

// traeSOLOHeadersWith 公共头组序，参数化版本/版本码：对话传 0.1.52，模型传 3.4.4。
func traeSOLOHeadersWith(req *http.Request, a *auth.Auth, stream bool, ideVer, ideVerCode string) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "Trae/"+ideVer)
	at := a.AccessTokenValue()
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", TraeAppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", ideVer)
	req.Header.Set("X-Ide-Version-Code", ideVerCode)
	req.Header.Set("X-App-Version-Code", ideVerCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "macos")
	req.Header.Set("X-OS-Version", traeOSVersion)
	req.Header.Set("X-Device-Brand", traeDeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// traeUgHeaders 设置签到/积分（api.trae.cn）所需头。
func traeUgHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+traeIdeVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessTokenValue())
	req.Header.Set("X-User-Region", "CN")
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// traeOAuthHeaders 设置 ExchangeToken 所需头（无签名，仅 UA）。
func traeOAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/"+traeIdeVersion)
}

// GetUserInfo 查询账号信息（登录补全用）：返回 uid / 昵称 / 企业 ID。
//
// 走 OAuth host 的 GetUserInfo（与 ExchangeToken 同域），用 access token 作
// X-Cloudide-Token。回调里带的 userInfo 可能缺字段，这里做权威补全；失败由调用
// 方决定是报错还是沿用回调里的值（本项目：uid 为空才判失败）。
func (c *TraeClient) GetUserInfo(ctx context.Context, a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	payload, err := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": traeIdeVersion})
	if err != nil {
		return "", "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+TraeEpUserInfo, bytes.NewReader(payload))
	if err != nil {
		return "", "", "", err
	}
	traeOAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", "", &Error{Kind: TraeClassify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateText(string(raw), 200)}
	}
	var out struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return out.Result.UserID, out.Result.ScreenName, out.Result.EnterpriseID, nil
}

// TraeClassify 按 HTTP 状态码 + body 判定错误类别，映射到本项目统一的 ErrKind。
// 1005 / 4008 = 权益或配额不足（HTTP 200 的流内码，或错误响应体）→ 硬冷却；
// 401 = session 失效；429 = 限流；404 = 上游偶发；5xx = 上游故障；其余 4xx = 客户端错误。
func TraeClassify(status int, body string) ErrKind {
	if strings.Contains(body, "1005") || strings.Contains(body, "4008") {
		return ErrHardCredit
	}
	switch {
	case status == http.StatusUnauthorized:
		return ErrSessionDead
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusNotFound:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// TraeWorkClassify 判定 Work 通道的失败形态（DESIGN §15.5 D8）。
//
// Work 与 SOLO 共用 HTTP 状态码语义，但另有自己的失败形态，不能套 SOLO 的分类：
//   - 400 + credit/1005/4008 文案：work_credits 余额不足 → 硬冷却（该通道额度耗尽，
//     等次日额度刷新；SOLO 出口不受影响）。
//   - 流内 / 响应体 code 4001 / 4000105：原生任务参数或配置错误 → 客户端错误
//     （trae2api 据此判定 native 失败；本项目 native-only，只能报给调用方）。
//   - 401/403：session 失效 → 禁用（Work 与 SOLO 共用凭证，所以是账号级禁用）。
//   - 429：限流 → 软冷却。
//
// 其余状态码回落到 TraeClassify（共用部分不重复实现）。
func TraeWorkClassify(status int, body string) ErrKind {
	if strings.Contains(body, "1005") || strings.Contains(body, "4008") ||
		strings.Contains(body, "credit") {
		return ErrHardCredit
	}
	if strings.Contains(body, `"code":4001`) || strings.Contains(body, `"code":4000105`) {
		return ErrClient
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrSessionDead
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	}
	return TraeClassify(status, body)
}

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 TraeClassify）、err 为 nil；
// 只有传输层失败才返回 err。ch 目前只影响日志/后续 Work 通道分流，SOLO 走同一端点。
func (c *TraeClient) ChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta, ch Channel) (io.ReadCloser, int, []byte, error) {
	// Work 通道走独立端点与原生 payload（trae_work.go）；SOLO 走 llm_utils_chat。
	if ch == ChannelWork {
		return c.chatStreamWork(ctx, a, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AgentHost+TraeEpChat, bytes.NewReader(TraePrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	traeSOLOHeaders(req, a, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 access token（refreshToken 轮换），
// 全程持 a.mu；成功即完成内存写回，落盘由调用方 SaveAtomic。
func (c *TraeClient) RefreshToken(ctx context.Context, a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	payload, _ := json.Marshal(map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+TraeEpExchange, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	traeOAuthHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &Error{Kind: TraeClassify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateText(string(raw), 200)}
	}
	var out struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if out.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = out.Result.Token
	if out.Result.RefreshToken != "" {
		a.RefreshToken = out.Result.RefreshToken
	}
	// 过期时间：优先 TokenExpireAt（上游返回毫秒，归一化为 Unix 秒）。
	if out.Result.TokenExpireAt > 0 {
		a.ExpiresAt = traeNormalizeExpiresAt(out.Result.TokenExpireAt)
	} else if out.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(out.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// traeNormalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒
// （毫秒时间戳 ~1.7e12，秒时间戳 ~1.7e9，用 1e12 区分）。
func traeNormalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// Checkin 执行每日签到：status 只用于判断是否已签到/是否开放；未签到且 enable
// 时 claim。claim 只执行一次，失败立即返回。
func (c *TraeClient) Checkin(ctx context.Context, a *auth.Auth) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UgHost+TraeEpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	traeUgHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &Error{Kind: TraeClassify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateText(string(raw), 200)}
	}
	var st struct {
		CheckedIn bool   `json:"checked_in"`
		Enable    bool   `json:"enable"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("checkin status parse: %w", err)
	}
	// 上游对失效凭证也回 HTTP 200：此时 code=1001、enable=false，
	// 只看 checked_in/enable 会把假 token 当成「签到未开放 → 幂等成功」。
	// 因此 code != 0 一律判失败，1001 归为 session 失效。
	if st.Code != 0 {
		kind := TraeClassify(resp.StatusCode, string(raw))
		if st.Code == 1001 {
			kind = ErrSessionDead
		}
		msg := st.Message
		if msg == "" {
			msg = truncateText(string(raw), 200)
		}
		return &Error{Kind: kind, Status: resp.StatusCode, Msg: msg}
	}
	if st.CheckedIn {
		return nil // 今天已签到：幂等成功
	}
	if !st.Enable {
		msg := st.Message
		if msg == "" {
			msg = truncateText(string(raw), 200)
		}
		return fmt.Errorf("checkin status: enable=false（签到未开放，未完成）: %s", msg)
	}

	_, err = c.claimCheckin(ctx, a)
	return err
}

// claimCheckin 单次 claim。9074 仍归类为 ErrSoftRate，但调用方不重试。
// 只有可解析且 code=0 才算成功。
func (c *TraeClient) claimCheckin(ctx context.Context, a *auth.Auth) (retry bool, err error) {
	claimReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UgHost+TraeEpCheckinClaim, bytes.NewReader([]byte(`{"req_source":2}`)))
	if err != nil {
		return false, err
	}
	traeUgHeaders(claimReq, a)
	claimResp, err := c.HTTP.Do(claimReq)
	if err != nil {
		return false, err
	}
	defer claimResp.Body.Close()
	claimRaw, _ := io.ReadAll(io.LimitReader(claimResp.Body, 1<<20))
	if claimResp.StatusCode >= 400 {
		return false, &Error{Kind: TraeClassify(claimResp.StatusCode, string(claimRaw)), Status: claimResp.StatusCode, Msg: truncateText(string(claimRaw), 200)}
	}
	var out struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(claimRaw, &out); err != nil {
		return false, fmt.Errorf("checkin claim parse: %w（body=%s）", err, truncateText(string(claimRaw), 200))
	}
	if out.Code == nil {
		return false, fmt.Errorf("checkin claim: missing code（body=%s）", truncateText(string(claimRaw), 200))
	}
	if *out.Code == 0 {
		return false, nil
	}
	kind := TraeClassify(claimResp.StatusCode, string(claimRaw))
	if *out.Code == 1001 {
		kind = ErrSessionDead
	} else if *out.Code == 9074 {
		kind = ErrSoftRate
	}
	return *out.Code == 9074, &Error{Kind: kind, Status: claimResp.StatusCode,
		Msg: fmt.Sprintf("checkin claim: %d %s", *out.Code, out.Message)}
}

// QueryCredits 聚合账号额度：remain = Σ(credits_limit) - Σ(credits_amount)，total = Σ limits。
func (c *TraeClient) QueryCredits(ctx context.Context, a *auth.Auth) (remain, total int64, err error) {
	_, remain, total, err = c.queryTraeCredits(ctx, a)
	return remain, total, nil
}

// traeEntPack 单个 entitlement 包的归一化结构（额度 + 时间 + 来源）。
type traeEntPack struct {
	Name      string // display_desc（如「签到奖励」「每月登录赠送」「免费」）
	Remain    int64
	Used      int64
	Size      int64
	StartAt   int64 // entitlement_base_info.start_time（Unix 秒，发放时刻）
	EndAt     int64 // entitlement_base_info.end_time（Unix 秒，缺失回落顶层 expire_time）
	ProductID int64 // entitlement_base_info.product_id（来源标识）
}

// parseTraeEntUsage 解析 Trae 额度接口（ide_user_ent_usage）的 entitlement 列表，
// 聚合 remain/total 并保留按包明细（供 CreditPackages 映射）。字段名取自实测样本：
// display_desc、entitlement_base_info.{quota.credits_limit,start_time,end_time,product_id}、
// usage.credits_amount（缺省 = 0）。credits_limit<=0 的权益类包（如免费档）跳过。
func parseTraeEntUsage(raw []byte) (packs []traeEntPack, remain, total int64, err error) {
	var out struct {
		UserEntitlementPackList []struct {
			DisplayDesc         string `json:"display_desc"`
			EntitlementBaseInfo struct {
				ProductID int64 `json:"product_id"`
				StartTime int64 `json:"start_time"`
				EndTime   int64 `json:"end_time"`
				Quota     struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
			ExpireTime int64 `json:"expire_time"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range out.UserEntitlementPackList {
		limit := p.EntitlementBaseInfo.Quota.CreditsLimit
		if limit <= 0 {
			continue
		}
		used := int64(p.Usage.CreditsAmount)
		total += limit
		remain += limit - used
		end := p.EntitlementBaseInfo.EndTime
		if end == 0 {
			end = p.ExpireTime
		}
		packs = append(packs, traeEntPack{
			Name:      p.DisplayDesc,
			Remain:    limit - used,
			Used:      used,
			Size:      limit,
			StartAt:   p.EntitlementBaseInfo.StartTime,
			EndAt:     end,
			ProductID: p.EntitlementBaseInfo.ProductID,
		})
	}
	return packs, remain, total, nil
}

// AccountCredits 带到期明细的聚合余额，口径与 WorkBuddy 的
// UserResourceDetailedWithExpiry 对齐：expiring = soon 窗口内到期的剩余积分，
// earliestAt/earliestRemaining = 最早到期包的时刻与剩余（同刻累加）。
func (c *TraeClient) AccountCredits(ctx context.Context, a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	packs, remain, total, err := c.queryTraeCredits(ctx, a)
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	now := time.Now()
	for _, p := range packs {
		if p.Remain <= 0 || p.EndAt <= 0 {
			continue
		}
		end := time.Unix(p.EndAt, 0)
		if !end.After(now) {
			continue
		}
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
			earliestRemaining = p.Remain
		} else if end.Equal(earliestAt) {
			earliestRemaining += p.Remain
		}
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += p.Remain
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// CreditPackages 套餐明细。复用额度接口的 entitlement 列表映射为 CreditPackage：
// name 取 display_desc、created_at/end_time 由 start_time/end_time 转 RFC3339、
// expires_at 用毫秒（前端按毫秒解析）、package_code 用「trae_<product_id>」。
func (c *TraeClient) CreditPackages(ctx context.Context, a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	packs, remain, total, err := c.queryTraeCredits(ctx, a)
	if err != nil {
		return nil, 0, 0, err
	}
	out := make([]CreditPackage, 0, len(packs))
	for _, p := range packs {
		out = append(out, CreditPackage{
			Name:        p.Name,
			Remain:      p.Remain,
			Used:        p.Used,
			Size:        p.Size,
			CreatedAt:   traeUnixRFC3339(p.StartAt),
			EndTime:     traeUnixRFC3339(p.EndAt),
			ExpiresAt:   p.EndAt * 1000, // 前端按 Unix 毫秒解析
			PackageCode: fmt.Sprintf("trae_%d", p.ProductID),
		})
	}
	return out, remain, total, nil
}

// traeUnixRFC3339 把 Unix 秒转东八区 RFC3339（前端只取日期/时间片段展示）；
// <=0 返回空串，前端按「无到期」处理。
func traeUnixRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
}

// queryTraeCredits 拉取并解析 Trae 额度接口，返回明细 + 聚合值（AccountCredits /
// CreditPackages 共用，避免重复请求）。
func (c *TraeClient) queryTraeCredits(ctx context.Context, a *auth.Auth) (packs []traeEntPack, remain, total int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UgHost+TraeEpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, 0, 0, err
	}
	traeUgHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, 0, 0, &Error{Kind: TraeClassify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateText(string(raw), 200)}
	}
	packs, remain, total, err = parseTraeEntUsage(raw)
	if err != nil {
		return nil, 0, 0, err
	}
	return packs, remain, total, nil
}

// truncateText 截断错误体，避免把整页 HTML 塞进日志/状态。
func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
