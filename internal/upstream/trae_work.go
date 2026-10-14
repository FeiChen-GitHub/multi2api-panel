// trae_work.go TRAE Work 通道（native）：create_agent_task 原生协议。
//
// 对照 trae2api/internal/upstream/work_client.go（已核实）：Work 走独立端点
// api5-normal.mchost.guru + /api/agent/v3/create_agent_task，payload 是原生任务体
// （不是 OpenAI 形状），消耗 work_credits。phase 1 只做 native，不做 bridge。
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// TraeWorkHost Work 通道专属端点（volc-dcdn）。
	TraeWorkHost = "https://api5-normal.mchost.guru"
	// TraeEpCreateAgentTask Work 通道端点。
	TraeEpCreateAgentTask = "/api/agent/v3/create_agent_task"
	// TraeWorkDefaultModel Work 通道默认模型。
	TraeWorkDefaultModel = "DeepSeek-V4-Flash-Official"
	// TraeWorkAppIDChat create_agent_task 使用的 AppID（trae2api WorkAppIDChat）。
	TraeWorkAppIDChat = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"

	traeWorkIdeVersion     = "0.1.63"
	traeWorkIdeVersionCode = "20260901"
)

// traeNewUUID 生成 8-4-4-4-12 形态的 UUIDv4（Work 请求体需要会话/消息 id）。
func traeNewUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// TraeExtractLastUserPrompt 取最后一条 user 消息的文本（字符串或 text part 数组）；
// 取不到回落 "你好"（trae2api 同口径，保证 payload 非空）。
func TraeExtractLastUserPrompt(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			if strings.TrimSpace(c) != "" {
				return c
			}
		case []any:
			var sb strings.Builder
			for _, part := range c {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok {
					sb.WriteString(t)
				}
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}
	return "你好"
}

// TraeBuildWorkPayload 构造 create_agent_task 的原生上行 JSON。
func TraeBuildWorkPayload(a *auth.Auth, model, prompt string) []byte {
	if strings.TrimSpace(model) == "" {
		model = TraeWorkDefaultModel
	}
	convID := traeNewUUID()
	sessID := traeNewUUID()
	msgID := traeNewUUID()

	queryJSON, _ := json.Marshal([]map[string]any{{
		"type": "text",
		"data": map[string]string{"content": prompt},
	}})

	// 上游内部模型名带 __dev 后缀；config_name 仍用对外名。
	internalModelName := model
	if !strings.HasSuffix(internalModelName, "__dev") {
		internalModelName = model + "__dev"
	}

	payload := map[string]any{
		"conversation_id": convID,
		"session_id":      sessID,
		"user_id":         a.UID,
		"device_id":       a.DeviceID,
		"agent_type":      TraeFunction,
		"model_name":      internalModelName,
		"config_name":     model,
		"ide_version":     traeWorkIdeVersion,
		"version_code":    20260901,
		"mode_type":       1,
		"plugin_channel":  "stable",
		"history_id_list": []string{},
		"user_input": map[string]any{
			"id":    msgID,
			"query": string(queryJSON),
			"messages": []map[string]any{{
				"role":    "user",
				"content": prompt,
			}},
		},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// traeWorkHeaders 注入 Work 通道的头（版本号与 SOLO 不同，AppID 用 chat 形态）。
func traeWorkHeaders(req *http.Request, a *auth.Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream, application/json")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "Trae/"+traeWorkIdeVersion)
	req.Header.Set("X-App-Id", TraeWorkAppIDChat)
	req.Header.Set("X-Ide-Version", traeWorkIdeVersion)
	req.Header.Set("X-Ide-Version-Code", traeWorkIdeVersionCode)
	req.Header.Set("X-App-Version-Code", traeWorkIdeVersionCode)
	req.Header.Set("X-Version-Code", traeWorkIdeVersionCode)
	req.Header.Set("X-Device-Type", "macos")
	req.Header.Set("X-Device-Platform", "darwin")
	req.Header.Set("X-Platform", "darwin")
	req.Header.Set("X-OS", "darwin")
	req.Header.Set("X-OSType", "darwin")
	req.Header.Set("X-System", "darwin")
	req.Header.Set("Request-Traffic-Type", "prod")
	if a != nil {
		at := a.AccessTokenValue()
		req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
		req.Header.Set("X-Cloudide-Token", at)
		req.Header.Set("X-Ide-Token", at)
		if a.UID != "" {
			req.Header.Set("X-Uid", a.UID)
		}
		if a.DeviceID != "" {
			req.Header.Set("X-Device-Id", a.DeviceID)
		}
		if a.MachineID != "" {
			req.Header.Set("X-Machine-Id", a.MachineID)
		}
	}
}

// chatStreamWork 发 Work 通道请求：解析 OpenAI body 取 model/messages，构造原生 payload，
// 返回原始 SSE 流（与 SOLO 同形状，便于上层复用同一套归一化与错误分类）。
func (c *TraeClient) chatStreamWork(ctx context.Context, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var reqBody struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
	}
	_ = json.Unmarshal(body, &reqBody)
	prompt := TraeExtractLastUserPrompt(reqBody.Messages)
	payload := TraeBuildWorkPayload(a, reqBody.Model, prompt)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TraeWorkHost+TraeEpCreateAgentTask, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	traeWorkHeaders(req, a, true)
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
