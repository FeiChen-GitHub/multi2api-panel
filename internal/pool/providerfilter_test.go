package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func mustParseAuth(t *testing.T, raw string) *auth.Auth {
	t.Helper()
	a, err := auth.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	return a
}

// 候选集必须按 provider 过滤：Trae 的请求不能拿到 WorkBuddy 的账号，反之亦然。
func TestAvailableKeysFiltersByProvider(t *testing.T) {
	p := New("")
	wb := mustParseAuth(t, `{"provider":"workbuddy","auth":{"accessToken":"wb"},"account":{"uid":"wb1"}}`)
	tr := mustParseAuth(t, `{"provider":"trae","auth":{"accessToken":"tr"},"account":{"uid":"tr1"}}`)
	p.Add(wb)
	p.Add(tr)

	keys := p.AvailableKeysForProviderModelRealm(auth.ProviderWorkBuddy, "", "")
	if len(keys) != 1 || keys[0] != wb.Key() {
		t.Fatalf("workbuddy keys = %v, want [%s]", keys, wb.Key())
	}
	keys = p.AvailableKeysForProviderModelRealm(auth.ProviderTrae, "", "")
	if len(keys) != 1 || keys[0] != tr.Key() {
		t.Fatalf("trae keys = %v, want [%s]", keys, tr.Key())
	}
}

// realm 过滤照旧：cn 请求不落 global 账号。realm 传空 = 不过滤。
func TestAvailableKeysFiltersByRealm(t *testing.T) {
	p := New("")
	cn := mustParseAuth(t, `{"provider":"workbuddy","auth":{"accessToken":"a","realm":"cn"},"account":{"uid":"cn1"}}`)
	gl := mustParseAuth(t, `{"provider":"workbuddy","auth":{"accessToken":"b","realm":"global"},"account":{"uid":"g1"}}`)
	p.Add(cn)
	p.Add(gl)

	if keys := p.AvailableKeysForProviderModelRealm(auth.ProviderWorkBuddy, "", "cn"); len(keys) != 1 || keys[0] != cn.Key() {
		t.Fatalf("cn keys = %v, want [%s]", keys, cn.Key())
	}
	if keys := p.AvailableKeysForProviderModelRealm(auth.ProviderWorkBuddy, "", "global"); len(keys) != 1 || keys[0] != gl.Key() {
		t.Fatalf("global keys = %v, want [%s]", keys, gl.Key())
	}
	if keys := p.AvailableKeysForProviderModelRealm(auth.ProviderWorkBuddy, "", ""); len(keys) != 2 {
		t.Fatalf("empty realm keys = %v, want both accounts", keys)
	}
}

// 不健康（暂停/禁用/冷却）的账号不进候选集。
func TestAvailableKeysSkipsPaused(t *testing.T) {
	p := New("")
	a := mustParseAuth(t, `{"provider":"workbuddy","auth":{"accessToken":"a"},"account":{"uid":"u1"}}`)
	b := mustParseAuth(t, `{"provider":"workbuddy","auth":{"accessToken":"b"},"account":{"uid":"u2"}}`)
	p.Add(a)
	p.Add(b)
	if ok := p.Pause("u2"); !ok {
		t.Fatal("Pause(u2) reported not found")
	}

	keys := p.AvailableKeysForProviderModelRealm(auth.ProviderWorkBuddy, "", "")
	if len(keys) != 1 || keys[0] != a.Key() {
		t.Fatalf("keys = %v, want only %s", keys, a.Key())
	}
}

// 显式 provider 路由：选号只在指定 provider 内，tried 排完后返回 nil，绝不跨 provider 兜底。
func TestPickExcludingForProviderRealmStaysWithinProvider(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	wb := mustParseAuth(t, `{"provider":"workbuddy","auth":{"accessToken":"wb"},"account":{"uid":"wb1"}}`)
	tr := mustParseAuth(t, `{"provider":"trae","auth":{"accessToken":"tr"},"account":{"uid":"tr1"}}`)
	p.Add(wb)
	p.Add(tr)

	for i := 0; i < 50; i++ {
		got := p.PickExcludingForProviderRealm(nil, "", "", auth.ProviderTrae)
		if got == nil || got.Provider() != auth.ProviderTrae {
			t.Fatalf("pick = %+v, want a trae account", got)
		}
	}
	got := p.PickExcludingForProviderRealm(map[auth.AccountKey]bool{tr.Key(): true}, "", "", auth.ProviderTrae)
	if got != nil {
		t.Fatalf("pick with tried trae = %+v, want nil（不跨 provider 兜底）", got)
	}
}
