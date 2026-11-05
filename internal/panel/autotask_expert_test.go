package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestExpertSpecOfParsesJumpURLID 「体验指定专家」任务的目标专家必须能从
// jump_url 的 deeplink 里解析出来（expert_use_* 系列都是这种形状）。
func TestExpertSpecOfParsesJumpURLID(t *testing.T) {
	task := &upstream.Task{
		TaskCode: "expert_use_maidehao",
		Title:    "体验「中国电商运营专家」",
		JumpURL:  "https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0&buddy_type=workbuddy",
	}
	sp := expertSpecOf(task)
	if sp.ID != "ex_GDl31WahyIH0" {
		t.Errorf("ID=%q want ex_GDl31WahyIH0", sp.ID)
	}
	if sp.Name != "中国电商运营专家" {
		t.Errorf("Name=%q want 中国电商运营专家", sp.Name)
	}
}

// TestExpertSpecOfFallsBackToTitle WorkBuddy_Valorant 的 jump_url 只有
// workbuddy://expert（不带 expertId），必须能靠标题里的专家名兜底。
func TestExpertSpecOfFallsBackToTitle(t *testing.T) {
	task := &upstream.Task{
		TaskCode: "WorkBuddy_Valorant",
		Title:    "体验「无畏契约拼豆专家」",
		TaskDesc: "即日起至11月15日，在专家页搜索并召唤「无畏契约拼豆专家」，完成一次对话。",
		JumpURL:  "workbuddy://expert",
	}
	sp := expertSpecOf(task)
	if sp.ID != "" {
		t.Errorf("ID=%q want empty（jump_url 未带 expertId）", sp.ID)
	}
	if sp.Name != "无畏契约拼豆专家" {
		t.Errorf("Name=%q want 无畏契约拼豆专家", sp.Name)
	}
}

// TestExpertSpecOfNilTask 空任务不得 panic（自动动作表里可能查不到任务）。
func TestExpertSpecOfNilTask(t *testing.T) {
	if sp := expertSpecOf(nil); sp.ID != "" || sp.Name != "" {
		t.Errorf("nil task should yield empty spec, got %+v", sp)
	}
}

// TestNewExpertTasksRegistered 「体验指定专家」系列必须都在动作表里登记，
// 否则 growthPending 会把它判成不可自动化、前端也不显示「一键完成」按钮。
func TestNewExpertTasksRegistered(t *testing.T) {
	for _, code := range []string{
		"WorkBuddy_Valorant",
		"expert_use_maidehao",
		"expert_use_douyinyin",
		"expert_use_shangjia_xuanchuan",
		"expert_use_bowangwang",
		"wb_wechat_oa_subscribe_task",
	} {
		if autoActionFor(code) == nil {
			t.Errorf("task %q missing from autoActions", code)
		}
	}
}

// TestExpertTaskSpecsAllRegistered 静态目标表里的每个任务码都必须登记在动作表，
// 且带非空 expertId——否则「一键完成」会静默跳过该任务。
func TestExpertTaskSpecsAllRegistered(t *testing.T) {
	if len(expertTaskSpecs) == 0 {
		t.Fatal("expertTaskSpecs 为空")
	}
	for code, sp := range expertTaskSpecs {
		if autoActionFor(code) == nil {
			t.Errorf("expertTaskSpecs 的 %q 未登记到 autoActions", code)
		}
		if sp.ID == "" || sp.Name == "" {
			t.Errorf("%q 的静态目标不完整: %+v", code, sp)
		}
	}
}

// TestResolveTaskExpertFallsBackToStaticID 市场列表未收录该专家时，必须用任务
// 下发的真实 expertId 继续（而不是直接放弃），否则新上线专家会永远点不亮。
func TestResolveTaskExpertFallsBackToStaticID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 市场搜索返回空列表（模拟专家尚未进入推荐列表）。
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"total_count": 0, "experts": []any{}},
		})
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	e, _, err := resolveTaskExpert(p, a, expertSpec{ID: "ex_GDl31WahyIH0", Name: "中国电商运营专家"})
	if err != nil {
		t.Fatalf("resolveTaskExpert: %v", err)
	}
	if e == nil {
		t.Fatal("市场未收录时应回落到任务下发的 expertId，而非放弃")
	}
	if e.ExpertID != "ex_GDl31WahyIH0" {
		t.Errorf("ExpertID=%q want ex_GDl31WahyIH0", e.ExpertID)
	}
	if e.ExpertType != "agent" {
		t.Errorf("ExpertType=%q want agent", e.ExpertType)
	}
}

// TestResolveTaskExpertByNameSearchesMarket 无 expertId 时（WorkBuddy_Valorant），
// 必须按任务标题里的专家名搜索市场并取名称完全一致者。
func TestResolveTaskExpertByNameSearchesMarket(t *testing.T) {
	var gotKeyword string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotKeyword, _ = body["keyword"].(string)
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"total_count": 2, "experts": []map[string]any{
				{"expert_id": "ex_OTHER", "expert_type": "agent", "display_name_zh": "近似专家", "version": "1.0.0"},
				{"expert_id": "ex_xQHHD1u5Jkua", "expert_type": "agent", "display_name_zh": "无畏契约拼豆专家", "version": "1.3.1"},
			}},
		})
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	e, how, err := resolveTaskExpert(p, a, expertSpec{Name: "无畏契约拼豆专家"})
	if err != nil {
		t.Fatalf("resolveTaskExpert: %v", err)
	}
	if e == nil {
		t.Fatal("应命中名称完全一致的专家")
	}
	if e.ExpertID != "ex_xQHHD1u5Jkua" {
		t.Errorf("ExpertID=%q want ex_xQHHD1u5Jkua（不能取近似的第一条）", e.ExpertID)
	}
	if gotKeyword != "无畏契约拼豆专家" {
		t.Errorf("keyword=%q want 无畏契约拼豆专家", gotKeyword)
	}
	if how == "" {
		t.Error("定位方式说明不应为空")
	}
}
