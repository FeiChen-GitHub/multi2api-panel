// autotask_expert.go 「体验某个指定专家」类成长任务的自动化。
//
// 背景（2026-11 上游新增任务）：成长中心上线了一批「体验「XX 专家」」单次任务
// （WorkBuddy_Valorant / expert_use_maidehao / expert_use_douyinyin /
// expert_use_shangjia_xuanchuan / expert_use_bowangwang）。它们的判据与既有的
// expert_5 完全同源——真实专家召唤链 + 真实 chat 的服务端 requestId +
// expert_actual_use 事件；差别只在**必须用任务点名的那位专家**：随便取市场
// 榜首（expert_5 的做法）不会计入。
//
// 因此本文件的动作按任务自身的 jump_url（deeplink 里的 expertId）与标题
// （「体验「X」」）定位目标专家，再用与 expert_5 同一条链路点亮。
// 2026-11-05 实测：五个任务在同一账号上逐条点亮并领奖到账。
package panel

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// expertSpec 任务指定的目标专家定位信息。
type expertSpec struct {
	ID   string // ex_ id（来自 jump_url 的 expertId 参数，可能为空）
	Name string // 展示名（来自任务标题/描述的「体验「X」」）
}

// expertTaskSpecs 「体验指定专家」任务的静态目标表（2026-11-05 从任务列表的
// jump_url 与标题抄录，同日实测点亮）。静态表的意义是**零额外上游请求**：
// 每个动作若都回头再拉一次任务列表，串行跑「一键完成」时容易把账号打进
// soft_rate（实测 25 项全量跑就撞过 429）。上游若改 id，resolveTaskExpert
// 会定位失败并安全跳过（不留半程态），届时改这张表即可。
var expertTaskSpecs = map[string]expertSpec{
	"WorkBuddy_Valorant":            {ID: "ex_xQHHD1u5Jkua", Name: "无畏契约拼豆专家"},
	"expert_use_maidehao":           {ID: "ex_GDl31WahyIH0", Name: "中国电商运营专家"},
	"expert_use_douyinyin":          {ID: "ex_FbY5nlWTqDod", Name: "抖音策略师"},
	"expert_use_shangjia_xuanchuan": {ID: "ex_Weo3x2EZuSDm", Name: "商家宣传页与海报设计师"},
	"expert_use_bowangwang":         {ID: "ex_WZY136wV5aMt", Name: "直播电商教练"},
}

// expertSpecOf 从任务的 jump_url、标题与描述解析目标专家。
//
// jump_url 是权威来源（多数任务的 deeplink 直接带 expertId）；但 WorkBuddy_Valorant
// 的 jump_url 只有 workbuddy://expert（无 id），这类任务只能靠标题里的专家名
// 去市场搜索定位，故 Name 也必须解析。
func expertSpecOf(t *upstream.Task) expertSpec {
	var sp expertSpec
	if t == nil {
		return sp
	}
	if u, err := url.Parse(t.JumpURL); err == nil {
		if q, qerr := url.ParseQuery(u.RawQuery); qerr == nil {
			if dl := q.Get("deeplink"); dl != "" {
				if du, derr := url.Parse(dl); derr == nil {
					sp.ID = du.Query().Get("expertId")
				}
			}
			if sp.ID == "" {
				sp.ID = q.Get("expertId")
			}
		}
	}
	// 名称：中文引号「」是任务文案的统一写法（「体验「X」」/「召唤「X」」）。
	for _, src := range []string{t.Title, t.TaskDesc, t.Description} {
		if src == "" || sp.Name != "" {
			continue
		}
		for _, kw := range []string{"体验", "召唤"} {
			i := strings.Index(src, kw)
			if i < 0 {
				continue
			}
			rest := src[i+len(kw):]
			s := strings.Index(rest, "「")
			if s < 0 {
				continue
			}
			if e := strings.Index(rest[s:], "」"); e > 0 {
				sp.Name = rest[s+len("「") : s+e]
				break
			}
		}
	}
	return sp
}

// expertSpecified 构造「体验指定专家」任务的执行函数（任务码在闭包里固定）。
//
// 目标专家定位顺序：
//  1. 静态表命中 → 直接用（零上游请求）；
//  2. 静态表没有 → 拉任务列表，从 jump_url（deeplink 的 expertId）与标题解析；
//  3. 市场里查不到但手上有 id → 用该 id 合成条目继续（id 来自任务下发，不是编造；
//     expert_actual_use 的计数校验只看 id 真实性 + requestId 的真实性）。
//
// 找不到专家时不做任何上报：避免留下「已 accept 未上报」的半程态。
func expertSpecified(code string) func(p *Panel, a *auth.Auth) (string, error) {
	return func(p *Panel, a *auth.Auth) (string, error) {
		sp, ok := expertTaskSpecs[code]
		if !ok {
			t, err := p.taskByCode(a, code)
			if err != nil {
				return "", err
			}
			if t == nil {
				return "该账号无此任务（可能已下线）", nil
			}
			sp = expertSpecOf(t)
		}
		e, how, err := resolveTaskExpert(p, a, sp)
		if err != nil {
			return "", err
		}
		if e == nil {
			return fmt.Sprintf("未定位到目标专家（id=%q name=%q），跳过以防半程态", sp.ID, sp.Name), nil
		}
		if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(*e)...); err != nil {
			return "", fmt.Errorf("召唤链: %w", err)
		}
		conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
		if err != nil {
			return "", fmt.Errorf("真实对话: %w", err)
		}
		events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
		for _, ev := range events {
			if ev["eventCode"] == "agent_task_created" {
				ev["has_expert"] = true
				ev["expert_id"] = e.ExpertID
				ev["expert_name"] = e.DisplayNameZH
				ev["expert_industry_id"] = ""
			}
		}
		events = append(events, upstream.DesktopExpertActualUseEvent(*e, conv, req))
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return "", fmt.Errorf("使用事件: %w", err)
		}
		return fmt.Sprintf("已对「%s」完成召唤+真实对话+使用上报（%s）", e.DisplayNameZH, how), nil
	}
}

// resolveTaskExpert 把 expertSpec 解析成市场里的专家条目。
// 返回 (专家, 定位方式说明, error)；专家为 nil 表示定位失败（调用方应跳过）。
func resolveTaskExpert(p *Panel, a *auth.Auth, sp expertSpec) (*upstream.MarketExpert, string, error) {
	if sp.ID != "" {
		e, err := p.cfg.Upstream.MarketExpertByID(a, sp.ID, sp.Name)
		if err != nil {
			return nil, "", fmt.Errorf("专家市场查询: %w", err)
		}
		if e != nil {
			return e, "按任务下发的 expertId 命中市场条目", nil
		}
		// 市场列表没收录（下架/分页外），但 id 是任务下发的真实 id：合成条目继续。
		name := sp.Name
		if name == "" {
			name = sp.ID
		}
		return &upstream.MarketExpert{
			ExpertID: sp.ID, ExpertType: "agent",
			DisplayNameZH: name, ProfessionZH: name, Version: "1.0.0",
		}, "市场未收录，用任务下发的 expertId", nil
	}
	if sp.Name == "" {
		return nil, "", nil
	}
	// 无 id：按名称搜索市场，取名称完全一致的第一条（关键字搜索会返回近似项）。
	list, err := p.cfg.Upstream.MarketExpertListKeyword(a, sp.Name)
	if err != nil {
		return nil, "", fmt.Errorf("专家市场搜索: %w", err)
	}
	for i := range list {
		if list[i].DisplayNameZH == sp.Name {
			return &list[i], "按任务标题的专家名搜索命中", nil
		}
	}
	for i := range list {
		if strings.Contains(list[i].DisplayNameZH, sp.Name) || strings.Contains(sp.Name, list[i].DisplayNameZH) {
			return &list[i], "按任务标题的专家名模糊命中", nil
		}
	}
	return nil, "", nil
}

// runWechatOA 完成 wb_wechat_oa_subscribe_task（关注公众号满 24 小时）。
//
// 判据是真实微信侧的关注回执 + 满 24 小时计时：服务端不接受客户端自报事件
// （2026-11-05 实测：/v2/report 无对应 eventCode，growth 域没有关注状态查询端点，
// 活动页 follow-guide 是纯静态页、无接口调用）。因此这里不做任何上报，
// 只返回可读指引；保留动作的意义是让前端给出准确说明，而不是笼统的「无对应接口」。
func runWechatOA(p *Panel, a *auth.Auth) (string, error) {
	return "该任务按真实微信关注回执计时（满 24 小时），无法用 API 代完成；请在微信扫码关注「腾讯 WorkBuddy」公众号，24 小时后回面板领奖", nil
}
