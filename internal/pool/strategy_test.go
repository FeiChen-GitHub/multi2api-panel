package pool

import (
	"encoding/json"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func orderedPool(t *testing.T, strategy string) *Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	for _, uid := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		p.Add(&auth.Auth{UID: uid})
	}
	for _, uid := range []string{"x", "y"} {
		p.Add(&auth.Auth{UID: uid, Domain: "workbuddy.ai"})
	}
	if err := p.SetScheduling(RegionStrategies{strategy, strategy}, RegionAccountOrder{}); err != nil {
		t.Fatal(err)
	}
	p.SetCostExploreInterval(0)
	return p
}
func wantPick(t *testing.T, p *Pool, tried map[string]bool, model, realm, uid string) {
	t.Helper()
	// 本仓 tried 的键是 AccountKey（uid 不再是身份），测试仍按 uid 书写，这里翻译一次。
	keys := map[auth.AccountKey]bool{}
	for uid := range tried {
		if a := p.AuthByUID(uid); a != nil {
			keys[a.Key()] = true
		}
	}
	a := p.PickExcludingForRealm(keys, model, realm)
	if a == nil {
		if uid != "" {
			t.Fatalf("pick nil; want %s", uid)
		}
		return
	}
	if a.UID != uid {
		t.Fatalf("pick %s; want %s", a.UID, uid)
	}
}
func TestRoundRobinAllAccountsAndIndependentCursors(t *testing.T) {
	p := orderedPool(t, RoundRobin)
	for i := 0; i < 21; i++ {
		wantPick(t, p, nil, "m", "cn", string(rune('a'+i%7)))
	}
	wantPick(t, p, nil, "m", "global", "x")
	wantPick(t, p, nil, "other", "cn", "a")
	wantPick(t, p, nil, "m", "global", "y")
	wantPick(t, p, nil, "m", "cn", "a")
	p.Pause("b")
	p.Disable("c", "test")
	p.CooldownSoftForModel("d", time.Hour, time.Now().Add(time.Hour), "m", "6004")
	p.SetMaxInFlight(1)
	if !p.Acquire("e") {
		t.Fatal("acquire")
	}
	wantPick(t, p, nil, "m", "cn", "f")
	p.Release("e")
	p.Revive("b")
	p.Revive("c")
	p.Revive("d")
	wantPick(t, p, nil, "m", "cn", "g")
	wantPick(t, p, nil, "m", "cn", "a")
	wantPick(t, p, nil, "m", "cn", "b")
}
func TestSequentialExhaustTemporaryAndModelIsolation(t *testing.T) {
	p := orderedPool(t, SequentialExhaust)
	for i := 0; i < 10; i++ {
		wantPick(t, p, nil, "m", "cn", "a")
	}
	p.NoteError("a")
	wantPick(t, p, map[string]bool{"a": true}, "m", "cn", "b")
	wantPick(t, p, nil, "m", "cn", "a")
	p.SetMaxInFlight(1)
	p.Acquire("a")
	wantPick(t, p, nil, "m", "cn", "b")
	p.Release("a")
	wantPick(t, p, nil, "m", "cn", "a")
	p.CooldownSoftForModel("a", time.Hour, time.Now().Add(time.Hour), "m", "6004")
	wantPick(t, p, nil, "m", "cn", "b")
	wantPick(t, p, nil, "other", "cn", "a")
	wantPick(t, p, nil, "m", "global", "x")
	p.Revive("a")
	wantPick(t, p, nil, "m", "cn", "b")
	p.Disable("b", "test")
	wantPick(t, p, nil, "m", "cn", "c")
}
func TestOrderedStrategiesRespectCostAndFloor(t *testing.T) {
	for _, strategy := range []string{RoundRobin, SequentialExhaust} {
		t.Run(strategy, func(t *testing.T) {
			p := orderedPool(t, strategy)
			p.SetCreditFloor(100)
			p.SetModelRateOf(func(realm, model string) string { return "1" })
			for uid := range p.byUID {
				p.SetCredits(uid, 50, 0)
			}
			p.NoteModelCost("c", "m", 0, 1000)
			wantPick(t, p, nil, "m", "cn", "c")
			p.CooldownSoftForModel("c", time.Hour, time.Now().Add(time.Hour), "m", "6004")
			wantPick(t, p, nil, "m", "cn", "")
			p.SetCredits("b", 200, 0)
			wantPick(t, p, nil, "m", "cn", "b")
		})
	}
}
func TestOrderedAllCoolingNoFallbackOrCrossRealm(t *testing.T) {
	for _, strategy := range []string{RoundRobin, SequentialExhaust} {
		t.Run(strategy, func(t *testing.T) {
			p := orderedPool(t, strategy)
			for _, uid := range []string{"a", "b", "c", "d", "e", "f", "g"} {
				p.Cooldown(uid, CoolSoft, time.Hour, "429")
			}
			wantPick(t, p, nil, "m", "cn", "")
			wantPick(t, p, nil, "m", "global", "x")
		})
	}
}
func TestOrderedCustomOrderHotUpdateAndMembership(t *testing.T) {
	p := orderedPool(t, RoundRobin)
	order := RegionAccountOrder{Domestic: []string{"c", "a", "c", "missing"}}
	p.SetScheduling(RegionStrategies{RoundRobin, SequentialExhaust}, order)
	order.Domestic[0] = "g" // 注入后外部修改不污染池。
	for _, uid := range []string{"c", "a", "b", "d", "e", "f", "g", "c"} {
		wantPick(t, p, nil, "m", "cn", uid)
	}
	p.Remove("c")
	wantPick(t, p, nil, "m", "cn", "a")
	p.Add(&auth.Auth{UID: "h"})
	p.SetScheduling(RegionStrategies{SequentialExhaust, RoundRobin}, RegionAccountOrder{})
	wantPick(t, p, nil, "m", "cn", "a")
	wantPick(t, p, nil, "m", "cn", "a")
	if p.SetScheduling(RegionStrategies{Domestic: "bad"}, RegionAccountOrder{}) == nil {
		t.Fatal("invalid strategy accepted")
	}
	wantPick(t, p, nil, "m", "cn", "a")
}
func TestOrderedPersistenceLocalAndSnapshot(t *testing.T) {
	for _, strategy := range []string{RoundRobin, SequentialExhaust} {
		t.Run(strategy, func(t *testing.T) {
			fp := filepath.Join(t.TempDir(), "state.json")
			p := New(fp)
			accounts := []*auth.Auth{{UID: "a"}, {UID: "b"}, {UID: "x", Domain: "workbuddy.ai"}, {UID: "y", Domain: "workbuddy.ai"}}
			p.SyncToDir(accounts)
			s := RegionStrategies{strategy, strategy}
			p.SetScheduling(s, RegionAccountOrder{})
			wantPick(t, p, nil, "m", "cn", "a")
			wantPick(t, p, nil, "m", "global", "x")
			p.CooldownSoftForModel("a", time.Hour, time.Now().Add(time.Hour), "blocked", "6004")
			p.Close()
			q := New(fp)
			defer q.Close()
			q.SyncToDir(accounts)
			q.SetScheduling(s, RegionAccountOrder{})
			want := "a"
			gwant := "x"
			if strategy == RoundRobin {
				want = "b"
				gwant = "y"
			}
			wantPick(t, q, nil, "m", "cn", want)
			wantPick(t, q, nil, "m", "global", gwant)
			wantPick(t, q, nil, "blocked", "cn", "b")
			// Redis 快照与本地格式共用，JSON 往返后恢复指针。
			p.mu.Lock()
			raw, err := json.Marshal(snapshot{stateFile: p.stateOverviewLocked(), SavedAt: time.Now()})
			p.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			var snap snapshot
			if err = json.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			r := New("")
			r.adoptSnapshot(snap)
			r.SyncToDir(accounts)
			r.SetScheduling(s, RegionAccountOrder{})
			wantPick(t, r, nil, "m", "cn", want)
		})
	}
}
func TestRoundRobinConcurrentFairnessAndLeases(t *testing.T) {
	p := orderedPool(t, RoundRobin)
	const n = 700
	got := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a := p.PickExcludingForRealm(nil, "m", "cn"); got <- a.UID }()
	}
	wg.Wait()
	close(got)
	counts := map[string]int{}
	for uid := range got {
		counts[uid]++
	}
	for _, uid := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if counts[uid] != 100 {
			t.Fatal(fmt.Sprintf("unfair counts: %v", counts))
		}
	}
	p.SetMaxInFlight(1)
	success := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); success <- p.Acquire("a") }()
	}
	wg.Wait()
	close(success)
	wins := 0
	for ok := range success {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("lease winners=%d", wins)
	}
	p.Release("a")
}
func TestSequentialCostExplorationDoesNotReplaceCurrent(t *testing.T) {
	p := orderedPool(t, SequentialExhaust)
	p.NoteModelCost("a", "m", 0, 1000)
	wantPick(t, p, nil, "m", "cn", "a")
	p.SetCostExploreInterval(time.Hour)
	wantPick(t, p, nil, "m", "cn", "b")
	wantPick(t, p, nil, "m", "cn", "a")
}

// 本仓扩展：调度游标带 provider 维度。同一 realm 下 WorkBuddy 与 Trae 是两套独立
// 池子，顺序耗尽的「主账号」不能跨 provider 继承——否则 Trae 请求会把 WorkBuddy
// 的游标推进到某个号上，两边互相干扰。
func TestSequentialExhaustCursorsIsolatedPerProvider(t *testing.T) {
	p := New("")
	wb1 := &auth.Auth{UID: "wb1", AccessToken: "a1", ExpiresAt: 9999999999}
	wb2 := &auth.Auth{UID: "wb2", AccessToken: "a2", ExpiresAt: 9999999999}
	tr1 := &auth.Auth{UID: "tr1", AccessToken: "a3", ExpiresAt: 9999999999}
	tr2 := &auth.Auth{UID: "tr2", AccessToken: "a4", ExpiresAt: 9999999999}
	auth.SetProvider(wb1, auth.ProviderWorkBuddy)
	auth.SetProvider(wb2, auth.ProviderWorkBuddy)
	auth.SetProvider(tr1, auth.ProviderTrae)
	auth.SetProvider(tr2, auth.ProviderTrae)
	for _, a := range []*auth.Auth{wb1, wb2, tr1, tr2} {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	if err := p.SetScheduling(RegionStrategies{
		Domestic: SequentialExhaust, International: WeightedRandom,
	}, RegionAccountOrder{Domestic: []string{"wb1", "wb2", "tr1", "tr2"}}); err != nil {
		t.Fatal(err)
	}

	pick := func(provider auth.Provider) string {
		a := p.PickExcludingForProviderRealm(nil, "m", "cn", provider)
		if a == nil {
			t.Fatalf("pick %s: nil", provider)
		}
		return a.UID
	}

	// WorkBuddy 钉在 wb1 上，反复选都是它。
	for i := 0; i < 5; i++ {
		if got := pick(auth.ProviderWorkBuddy); got != "wb1" {
			t.Fatalf("workbuddy pick #%d = %s; want wb1（顺序耗尽应钉住）", i, got)
		}
	}
	// Trae 有自己的游标，从 tr1 开始，不受 WorkBuddy 已推进的影响。
	if got := pick(auth.ProviderTrae); got != "tr1" {
		t.Fatalf("trae pick = %s; want tr1（游标按 provider 隔离）", got)
	}
	// wb1 冷却后才轮到 wb2——且 Trae 侧不受牵连。
	p.Cooldown("wb1", CoolSoft, time.Hour, "test")
	if got := pick(auth.ProviderWorkBuddy); got != "wb2" {
		t.Fatalf("workbuddy after wb1 cooled = %s; want wb2", got)
	}
	if got := pick(auth.ProviderTrae); got != "tr1" {
		t.Fatalf("trae after wb1 cooled = %s; want tr1（不得跨 provider 牵连）", got)
	}
}

// 有序策略不使用全冷却兜底：全部候选冷却时直接返回 nil，等待恢复，
// 而不是像加权随机那样把冷却号捞回来（那会破坏有序策略的确定性）。
func TestOrderedStrategyNoCooldownFallback(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "only", AccessToken: "at", ExpiresAt: 9999999999}
	p.Add(a)
	p.SetCredits("only", 1000, 0)
	if err := p.SetScheduling(RegionStrategies{Domestic: RoundRobin, International: WeightedRandom}, RegionAccountOrder{}); err != nil {
		t.Fatal(err)
	}
	p.Cooldown("only", CoolSoft, time.Hour, "test")

	if got := p.PickExcludingForRealm(nil, "m", "cn"); got != nil {
		t.Fatalf("ordered strategy must not fall back to cooling accounts; got %s", got.UID)
	}
	// 加权随机保留原兜底（零回归对照）。
	p2 := New("")
	a2 := &auth.Auth{UID: "only", AccessToken: "at", ExpiresAt: 9999999999}
	p2.Add(a2)
	p2.SetCredits("only", 1000, 0)
	p2.Cooldown("only", CoolSoft, time.Hour, "test")
	if got := p2.PickExcludingForRealm(nil, "m", "cn"); got == nil {
		t.Fatal("weighted_random should keep the all-cooled fallback")
	}
}
