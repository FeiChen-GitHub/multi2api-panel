package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// lockedTasksBackend 上游任务列表：只下发一条 locked 的 Sequential_Tasks_2
// （2026-11 实测形态：未 unlock 时 target=0、accept_status=not_accepted）。
// otherReqs 统计「列表以外的任何上游请求」——locked 任务不应产生这些请求。
type lockedTasksBackend struct {
	otherReqs atomic.Int32
	listReqs  atomic.Int32
}

func (b *lockedTasksBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/activity/growth/tasks" {
			b.listReqs.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{
						"task_code": "Sequential_Tasks_2", "title": "完成 1 次专家对话",
						"task_desc":     "在小程序内选中专家并完成有效对话",
						"tag":           "小程序限定",
						"locked":        true,
						"target":        0,
						"current":       0,
						"accept_status": "not_accepted",
						"reward_credit": 200, "reward_energy": 5,
					},
				}},
			})
			return
		}
		b.otherReqs.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
	}
}

// TestRunAutoAllSkipsLockedTask 「一键完成全部」遇到上游 locked 任务必须直接跳过，
// 不能跑动作（专家市场查询 / accept / 事件上报都属于白耗配额）。
//
// 背景（2026-11-05）：Sequential 链每日零点解锁一环，未解锁时 accept 不落账、
// 行为事件也不计分；旧实现会照跑 runMiniExpert，每轮多打数个上游请求并回报
// 「accept 未登记生效」，正是限流雪崩的残留来源。
func TestRunAutoAllSkipsLockedTask(t *testing.T) {
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })

	b := &lockedTasksBackend{}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	out := p.runAutoAll(&auth.Auth{AccessToken: "at", UID: "u1"})

	if got := b.otherReqs.Load(); got != 0 {
		t.Errorf("locked 任务产生了 %d 次列表以外的上游请求，want 0（应直接跳过）", got)
	}
	var hit map[string]any
	for _, item := range out {
		if item["task_code"] == "Sequential_Tasks_2" {
			hit = item
		}
	}
	if hit == nil {
		t.Fatalf("runAutoAll 未回执 Sequential_Tasks_2，out=%v", out)
	}
	if hit["status"] != "skipped" {
		t.Errorf("status = %v, want skipped", hit["status"])
	}
	if msg, _ := hit["message"].(string); !strings.Contains(msg, "未解锁") {
		t.Errorf("message = %q，应说明上游未解锁", msg)
	}
}

// TestAccountTaskAutoSkipsLockedTask 单任务「一键完成」入口对 locked 任务返回
// 200 + skipped，且不向上游发任何动作请求（只允许一次任务列表查询）。
func TestAccountTaskAutoSkipsLockedTask(t *testing.T) {
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })

	b := &lockedTasksBackend{}
	srv := httptest.NewServer(b.handler())
	defer srv.Close()

	pl := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	pl.Add(a)
	key := string(a.Key())
	if key == "" {
		t.Fatal("账号未生成 AccountKey")
	}
	p := &Panel{cfg: Config{
		Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	}}

	body := strings.NewReader(`{"task_code":"Sequential_Tasks_2"}`)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/"+key+"/tasks/auto", body)
	req.SetPathValue("key", key)
	rec := httptest.NewRecorder()
	p.accountTaskAuto(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200；body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body.String())
	}
	if resp["skipped"] != true {
		t.Errorf("skipped = %v, want true；resp=%v", resp["skipped"], resp)
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "未解锁") {
		t.Errorf("message = %q，应说明上游未解锁", msg)
	}
	if got := b.otherReqs.Load(); got != 0 {
		t.Errorf("locked 任务产生了 %d 次列表以外的上游请求，want 0", got)
	}
}
