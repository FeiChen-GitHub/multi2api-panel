package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunAutoAllUsesSnapshotForPrecheck 「一键完成」的前置判断必须只读一次任务
// 快照，不能每项都重新拉全量列表。
//
// 背景（2026-11-05 实测）：旧实现对 autoActions 里每个任务各调一次 ListTasks，
// 25 项串行跑就把账号打进上游 429 soft_rate，用户看到一片「查询失败」，
// 新任务也就永远推进不了。这里断言列表请求次数与任务数量无关。
func TestRunAutoAllUsesSnapshotForPrecheck(t *testing.T) {
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })

	var listCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			atomic.AddInt32(&listCalls, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "chat_5", "title": "对话 5 次", "target": 5, "current": 5,
						"accept_status": "claimed", "reward_credit": 100, "reward_energy": 5},
				}},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	out := p.runAutoAll(&auth.Auth{AccessToken: "at", UID: "u1"})

	if len(out) != len(autoActions) {
		t.Fatalf("结果项数 = %d, want %d（每项都要有回执）", len(out), len(autoActions))
	}
	// 1 次默认口径 + 1 次 mp 口径；不再随任务数线性增长。
	if got := atomic.LoadInt32(&listCalls); got > 4 {
		t.Errorf("列表请求 %d 次，want <= 4（前置判断应读快照，不逐项重拉）", got)
	}
}

// TestRunAutoAllStopsWhenRateLimited 撞上游 429 时必须停止后续上游调用，
// 并把余下项明确标成「限流提前结束」，而不是逐项打成失败。
func TestRunAutoAllStopsWhenRateLimited(t *testing.T) {
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })

	var claimCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					// 三个可达标任务：首个领奖即被限流，其余应直接停手。
					{"task_code": "chat_5", "target": 5, "current": 5, "accept_status": "accepted"},
					{"task_code": "first_buddy", "target": 1, "current": 1, "accept_status": "accepted"},
					{"task_code": "Library_read", "target": 1, "current": 1, "accept_status": "accepted"},
				}},
			})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			atomic.AddInt32(&claimCalls, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]any{"error_msg": "Too Many Requests"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	out := p.runAutoAll(&auth.Auth{AccessToken: "at", UID: "u1"})

	if got := atomic.LoadInt32(&claimCalls); got != 1 {
		t.Errorf("claim 调用 %d 次，want 1（限流后必须收手）", got)
	}
	var limited int
	for _, item := range out {
		if msg, _ := item["message"].(string); strings.Contains(msg, "限流") {
			limited++
		}
	}
	if limited == 0 {
		t.Errorf("应有条目说明因限流提前结束，out=%v", out)
	}
}
