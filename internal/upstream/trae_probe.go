// trae_probe.go TRAE 双通道额度探针（DESIGN §6.6 ProbeCredits）。
//
// 对照 trae2api/internal/upstream/work_client.go 的 ProbeCredits：往 SOLO 端点发一个
// 极小的 ping 请求，从首帧的 cn_credits_remain_info 里读出 ide_credits（SOLO 额度）
// 与 work_credits（Work 额度）。探测是**读**语义——ping 几乎不消耗额度，换来的是
// 两条通道各自的真实余额，供选号与冷却解冻使用。
//
// 没有它，Work 通道就只剩「撞到失败才知道没钱」这一条路：workCredits 恒为 0，
// 选号退化成「取 key 最小者」，且 work_credits 恢复后无法自动解冻。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TraeCreditsSnapshot 双通道额度快照（浮点，与上游 credits 量纲一致）。
type TraeCreditsSnapshot struct {
	// IdeCredits SOLO 通道余额（cn_credits_remain_info.ide_credits）。
	IdeCredits float64
	// WorkCredits Work 通道余额（cn_credits_remain_info.work_credits）。
	WorkCredits float64
}

// ProbeCredits 探测账号双通道余额。命中 cn_credits_remain_info 即返回；上游未给该
// 字段（端点变更/降级）返回错误，由调用方决定重试节奏——探测失败**不得**把余额写成
// 0，否则会把健康号误判成余额耗尽而冷却掉。
func (c *TraeClient) ProbeCredits(ctx context.Context, a *auth.Auth) (*TraeCreditsSnapshot, error) {
	payload, err := json.Marshal(map[string]any{
		"function":    TraeFunction,
		"config_name": TraeWorkDefaultModel,
		"model":       TraeWorkDefaultModel,
		"stream":      true,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": "ping"}},
		}},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AgentHost+TraeEpChat, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// useChatAppID=true：与 trae2api 一致，探测走 chat 形态的 AppID。
	traeSOLOHeaders(req, a, true)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe credits transport error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: TraeClassify(resp.StatusCode, ""), Status: resp.StatusCode, Msg: "probe credits http " + http.StatusText(resp.StatusCode)}
	}

	// 只读前若干行找额度帧：ping 的首帧通常就带着 cn_credits_remain_info，
	// 找到即返回，不必读完整条流（探测要快，且不该为一次余额查询挂住长流）。
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var data map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data) != nil {
			continue
		}
		remain, ok := data["cn_credits_remain_info"].(map[string]any)
		if !ok {
			continue
		}
		var snap TraeCreditsSnapshot
		if v, ok := remain["ide_credits"].(float64); ok {
			snap.IdeCredits = v
		}
		if v, ok := remain["work_credits"].(float64); ok {
			snap.WorkCredits = v
		}
		return &snap, nil
	}
	return nil, fmt.Errorf("no cn_credits_remain_info in probe response")
}
