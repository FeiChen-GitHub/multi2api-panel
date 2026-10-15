package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func traeAuthForProbe() *auth.Auth {
	return &auth.Auth{AccessToken: "jwt-1", UID: "u1", DeviceID: "1234567890123456"}
}

// ProbeCredits 从首帧的 cn_credits_remain_info 读出双通道余额。
func TestTraeProbeCreditsParsesBothChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpChat {
			t.Errorf("path = %s, want %s", r.URL.Path, TraeEpChat)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"billing_mode\":\"credits\",\"cn_credits_remain_info\":{\"ide_credits\":12.5,\"work_credits\":1987.0284}}\n\n"))
	}))
	defer srv.Close()

	c := NewTraeClient()
	c.AgentHost = srv.URL
	snap, err := c.ProbeCredits(context.Background(), traeAuthForProbe())
	if err != nil {
		t.Fatalf("ProbeCredits: %v", err)
	}
	if snap.IdeCredits != 12.5 {
		t.Errorf("ide_credits = %v, want 12.5", snap.IdeCredits)
	}
	if snap.WorkCredits != 1987.0284 {
		t.Errorf("work_credits = %v, want 1987.0284", snap.WorkCredits)
	}
}

// 上游没有给额度帧时**必须报错**，不能返回零值快照——否则调用方会把
// 健康号当成余额耗尽而误冷却（探测失败 ≠ 余额为 0）。
func TestTraeProbeCreditsFailsWithoutRemainInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data: {\"response\":\"hi\"}\n\n"))
	}))
	defer srv.Close()

	c := NewTraeClient()
	c.AgentHost = srv.URL
	snap, err := c.ProbeCredits(context.Background(), traeAuthForProbe())
	if err == nil {
		t.Fatalf("ProbeCredits = %+v, want error when remain info missing", snap)
	}
}

// 非 2xx 探测返回 *Error（带分类），供调用方区分 session 失效与限流。
func TestTraeProbeCreditsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewTraeClient()
	c.AgentHost = srv.URL
	_, err := c.ProbeCredits(context.Background(), traeAuthForProbe())
	if err == nil {
		t.Fatal("want error on 401")
	}
	ue, ok := err.(*Error)
	if !ok || ue.Kind != ErrSessionDead {
		t.Fatalf("err = %v (%T), want *Error kind ErrSessionDead", err, err)
	}
}

// Work 通道的错误码映射（DESIGN §15.5 D8）：不能套 SOLO 的分类。
func TestTraeWorkClassify(t *testing.T) {
	cases := []struct {
		name string
		st   int
		body string
		want ErrKind
	}{
		{"work_credits 不足（credit 文案）", http.StatusBadRequest, `{"error":"insufficient credit"}`, ErrHardCredit},
		{"work_credits 不足（1005）", http.StatusBadRequest, `{"code":1005}`, ErrHardCredit},
		{"work_credits 不足（4008）", http.StatusBadRequest, `{"code":4008}`, ErrHardCredit},
		{"原生任务参数错误 4001", http.StatusBadRequest, `{"code":4001,"message":"failed to get summary config"}`, ErrClient},
		{"原生任务配置错误 4000105", http.StatusBadRequest, `{"code":4000105}`, ErrClient},
		{"session 失效 401", http.StatusUnauthorized, ``, ErrSessionDead},
		{"session 失效 403", http.StatusForbidden, ``, ErrSessionDead},
		{"限流 429", http.StatusTooManyRequests, ``, ErrSoftRate},
		{"上游故障 5xx", http.StatusBadGateway, ``, ErrServer},
		{"404", http.StatusNotFound, ``, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraeWorkClassify(tc.st, tc.body); got != tc.want {
				t.Fatalf("TraeWorkClassify(%d, %q) = %v, want %v", tc.st, tc.body, got, tc.want)
			}
		})
	}
}

// 关键差异：同样的 400 + credit 文案，SOLO 分类与 Work 分类在这里一致（都是硬冷却），
// 但 4001 在 SOLO 侧是普通客户端错误、在 Work 侧同样是客户端错误 —— 真正的差异是
// 冷却口径（Work 只冷通道）。此处钉住「不误判成账号级 session/服务端故障」。
func TestTraeWorkClassifyDoesNotOverEscalate(t *testing.T) {
	if got := TraeWorkClassify(http.StatusBadRequest, `{"code":4001}`); got == ErrSessionDead || got == ErrServer {
		t.Fatalf("4001 must not escalate to session dead / server error, got %v", got)
	}
}
