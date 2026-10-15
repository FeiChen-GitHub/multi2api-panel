package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// mustParseTrae 解析一个 Trae 账号。key 非空时用它作 AccountKey（模拟「同一个
// 账号跨重启重新加载」——否则每次解析都会生成新的 UUID，持久化状态无法匹配）。
func mustParseTrae(t *testing.T, uid string, key ...auth.AccountKey) *auth.Auth {
	t.Helper()
	raw := `{"provider":"trae","auth":{"accessToken":"` + uid + `"},"account":{"uid":"` + uid + `"}`
	if len(key) > 0 && key[0] != "" {
		raw += `,"account_key":"` + string(key[0]) + `"`
	}
	return mustParseAuth(t, raw+`}`)
}

// SOLO 撞限额冷却账号后，Work 通道仍必须能选到这个账号（DESIGN §6.6 第 ③ 点）。
// 这是「通道维度」的核心断言：没有它，一次 SOLO 的 1005 会把 Work 出口一起封死。
func TestWorkChannelNotBlockedBySoloCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	a := mustParseTrae(t, "u1")
	p.Add(a)

	p.CooldownUntilTomorrow4AM("u1", "trae 权益/配额不足")

	// SOLO 侧：账号级冷却生效，选不到号。
	if got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, ChannelSOLO); got != nil {
		t.Fatalf("SOLO pick = %+v, want nil（账号级冷却中）", got)
	}
	// Work 侧：不受影响。
	got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, ChannelWork)
	if got == nil || got.UID != "u1" {
		t.Fatalf("Work pick = %+v, want u1（Work 通道不被 SOLO 冷却连坐）", got)
	}
}

// 反过来：Work 通道冷却不该影响 SOLO 出口。
func TestSoloChannelNotBlockedByWorkCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	a := mustParseTrae(t, "u1")
	p.Add(a)

	p.CooldownWork("u1", time.Hour, TraeWorkCreditReason())

	if got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, ChannelWork); got != nil {
		t.Fatalf("Work pick = %+v, want nil（Work 通道冷却中）", got)
	}
	got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, ChannelSOLO)
	if got == nil || got.UID != "u1" {
		t.Fatalf("SOLO pick = %+v, want u1（SOLO 不被 Work 冷却连坐）", got)
	}
}

// Work 通道选号按 work_credits 最高优先，而不是 SOLO 的 credits（DESIGN §9.5）。
func TestWorkChannelPicksByWorkCredits(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	soloRich := mustParseTrae(t, "solo-rich")
	workRich := mustParseTrae(t, "work-rich")
	p.Add(soloRich)
	p.Add(workRich)
	// 故意让两边的余额排序相反：SOLO 余额最高的是 soloRich，Work 余额最高的是 workRich。
	p.SetCredits("solo-rich", 900, 0)
	p.SetCredits("work-rich", 100, 0)
	p.SetWorkCredits("solo-rich", 1)
	p.SetWorkCredits("work-rich", 500)

	for i := 0; i < 20; i++ {
		got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, ChannelWork)
		if got == nil || got.UID != "work-rich" {
			t.Fatalf("Work pick = %+v, want work-rich（按 work_credits）", got)
		}
	}
	// SOLO 侧仍按 credits。
	for i := 0; i < 20; i++ {
		got := p.PickExcludingForProviderRealmChannel(nil, "", "cn", auth.ProviderTrae, ChannelSOLO)
		if got == nil || got.UID != "solo-rich" {
			t.Fatalf("SOLO pick = %+v, want solo-rich（按 credits）", got)
		}
	}
}

// work_credits 回正即自动解冻 Work 冷却（只解「余额不足」这类，不误清其他原因）。
func TestSetWorkCreditsRevivesOnlyCreditCooldown(t *testing.T) {
	p := New("")
	a := mustParseTrae(t, "u1")
	p.Add(a)

	p.CooldownWork("u1", time.Hour, TraeWorkCreditReason())
	p.SetWorkCredits("u1", 42)
	if p.WorkCredits("u1") != 42 {
		t.Fatalf("work credits = %v, want 42", p.WorkCredits("u1"))
	}
	st, _ := p.Status("u1")
	if st.WorkCooling {
		t.Fatal("Work 冷却应随余额回正自动解冻")
	}

	// 其他原因的冷却不被余额刷新抹掉（限流的恢复证据是重置墙钟，不是余额）。
	p.CooldownWork("u1", time.Hour, "trae work 限流")
	p.SetWorkCredits("u1", 7)
	st, _ = p.Status("u1")
	if !st.WorkCooling {
		t.Fatal("非余额原因的 Work 冷却不得被余额刷新清除")
	}
}

// Work 通道状态必须能跨重启保留（state.json 持久化）。
func TestWorkChannelStatePersists(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	a := mustParseTrae(t, "u1")
	p.Add(a)
	key := a.Key() // 重启后必须用同一个主键加载，否则状态归属到新账号
	p.SetWorkCredits("u1", 123.5)
	p.CooldownWork("u1", time.Hour, TraeWorkCreditReason())
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(mustParseTrae(t, "u1", key))
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("u1 not restored")
	}
	if st.WorkCredits != 123.5 {
		t.Fatalf("work credits = %v, want 123.5（未持久化）", st.WorkCredits)
	}
	if !st.WorkCooling {
		t.Fatal("Work 冷却未跨重启保留")
	}
}

// 已过期的 Work 冷却不得在恢复时被复活（与 breakerUntil 同口径）。
func TestExpiredWorkCooldownNotRestored(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	a := mustParseTrae(t, "u1")
	p.Add(a)
	key := a.Key()
	p.mu.Lock()
	if e, ok := p.byUID["u1"]; ok {
		e.workUntil = time.Now().Add(-time.Minute) // 写一条已过期的冷却
		e.workReason = TraeWorkCreditReason()
	}
	p.mu.Unlock()
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(mustParseTrae(t, "u1", key))
	st, _ := p2.Status("u1")
	if st.WorkCooling {
		t.Fatal("已过期的 Work 冷却不应在恢复后复活")
	}
}

// 运维「解冻」是全清：Work 通道冷却一并解除，否则按钮看起来像没生效。
func TestReviveClearsWorkCooldown(t *testing.T) {
	p := New("")
	a := mustParseTrae(t, "u1")
	p.Add(a)
	p.CooldownWork("u1", time.Hour, TraeWorkCreditReason())
	if ok := p.Revive("u1"); !ok {
		t.Fatal("Revive reported not found")
	}
	st, _ := p.Status("u1")
	if st.WorkCooling {
		t.Fatal("Revive 后 Work 冷却应被清除")
	}
}
