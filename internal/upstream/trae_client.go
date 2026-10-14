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
	TraeClientID  = "en1oxy7wnw8j9n"
	TraeAppID     = "6eefa01c-1036-4c7e-9e5a-d891f63bfcd8"

	traeIdeVersion     = "0.1.52"
	traeIdeVersionCode = "20260811"
	traeOSVersion      = "macOS 15.7.4"
	traeDeviceBrand    = "Apple"

	TraeEpChat          = "/api/agent/v3/llm_utils_chat"
	TraeEpModels        = "/api/ide/v1/get_detail_param"
	TraeEpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
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

// traeSOLOHeaders 设置 llm_utils_chat / get_detail_param 所需头（实测必须）。
func traeSOLOHeaders(req *http.Request, a *auth.Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "Trae/"+traeIdeVersion)
	at := a.AccessTokenValue()
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", TraeAppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", traeIdeVersion)
	req.Header.Set("X-Ide-Version-Code", traeIdeVersionCode)
	req.Header.Set("X-App-Version-Code", traeIdeVersionCode)
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

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 TraeClassify）、err 为 nil；
// 只有传输层失败才返回 err。ch 目前只影响日志/后续 Work 通道分流，SOLO 走同一端点。
func (c *TraeClient) ChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta, ch Channel) (io.ReadCloser, int, []byte, error) {
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

// Checkin 执行每日签到：先查 status，未签到且 enable 才 claim（幂等）。
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
		CheckedIn bool `json:"checked_in"`
		Enable    bool `json:"enable"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("checkin status parse: %w", err)
	}
	if st.CheckedIn || !st.Enable {
		return nil // 已签到 / 签到未开放：幂等成功
	}
	claimReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UgHost+TraeEpCheckinClaim, bytes.NewReader([]byte(`{"req_source":2}`)))
	if err != nil {
		return err
	}
	traeUgHeaders(claimReq, a)
	claimResp, err := c.HTTP.Do(claimReq)
	if err != nil {
		return err
	}
	defer claimResp.Body.Close()
	claimRaw, _ := io.ReadAll(io.LimitReader(claimResp.Body, 1<<20))
	if claimResp.StatusCode >= 400 {
		return &Error{Kind: TraeClassify(claimResp.StatusCode, string(claimRaw)), Status: claimResp.StatusCode, Msg: truncateText(string(claimRaw), 200)}
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(claimRaw, &out); err == nil && out.Code != 0 {
		return fmt.Errorf("checkin claim: %d %s", out.Code, out.Message)
	}
	return nil
}

// QueryCredits 聚合账号额度：remain = Σ(credits_limit) - Σ(credits_amount)，total = Σ limits。
func (c *TraeClient) QueryCredits(ctx context.Context, a *auth.Auth) (remain, total int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.UgHost+TraeEpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, 0, err
	}
	traeUgHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return 0, 0, &Error{Kind: TraeClassify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateText(string(raw), 200)}
	}
	var out struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range out.UserEntitlementPackList {
		limit := p.EntitlementBaseInfo.Quota.CreditsLimit
		if limit <= 0 {
			continue
		}
		used := int64(p.Usage.CreditsAmount)
		total += limit
		remain += limit - used
	}
	return remain, total, nil
}

// truncateText 截断错误体，避免把整页 HTML 塞进日志/状态。
func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
