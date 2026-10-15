package session

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 旧版粘性绑定存的是裸 uid：启动时必须原地改写成 AccountKey（并写回 store），
// 否则这些绑定永远匹配不到账号，存量会话会在升级后集体换号（上游 prompt cache 全 miss）。
func TestLoadFromStoreMigratesLegacyUIDBinds(t *testing.T) {
	const newKey = "11111111-1111-4111-8111-111111111111"
	st := newCountingStore()
	st.binds["c1"] = "old-uid-1" // 旧值：裸 uid
	st.binds["c2"] = newKey      // 新值：AccountKey，原样用

	resolved := map[string]auth.AccountKey{"old-uid-1": auth.AccountKey(newKey)}
	r := New(Config{
		TTL:        time.Minute,
		Store:      st,
		Available:  func() []string { return []string{newKey} },
		ResolveLegacyKey: func(uid string) (auth.AccountKey, bool) {
			k, ok := resolved[uid]
			return k, ok
		},
	})
	r.LoadFromStore()

	if r.Count() != 2 {
		t.Fatalf("restored count=%d, want 2（新旧值都应载入）", r.Count())
	}
	if got := st.binds["c1"]; got != newKey {
		t.Fatalf("旧绑定未原地改写: c1=%q, want %q", got, newKey)
	}
	if k, ok := r.Resolve("c1"); !ok || k != newKey {
		t.Fatalf("c1 解析 = %q/%v, want %q", k, ok, newKey)
	}
}

// 解析不到的旧值（账号已删/换号）按失效处理：整条绑定丢弃，不留下永远不可用的脏绑定。
func TestLoadFromStoreDropsUnresolvableLegacyBinds(t *testing.T) {
	const newKey = "22222222-2222-4222-8222-222222222222"
	st := newCountingStore()
	st.binds["c1"] = "deleted-uid"
	st.binds["c2"] = newKey

	r := New(Config{
		TTL:              time.Minute,
		Store:            st,
		Available:        func() []string { return []string{newKey} },
		ResolveLegacyKey: func(string) (auth.AccountKey, bool) { return "", false },
	})
	r.LoadFromStore()

	if r.Count() != 1 {
		t.Fatalf("restored count=%d, want 1（只有新值那条）", r.Count())
	}
	if _, ok := st.binds["c1"]; ok {
		t.Fatal("无法解析的旧绑定应被删除")
	}
	if st.delBinds != 1 {
		t.Fatalf("DelBind 次数 = %d, want 1", st.delBinds)
	}
}

// 未注入迁移函数（裸用/纯内存场景）时保持旧行为：值原样接受，不擅自判死。
func TestLoadFromStoreWithoutMigratorKeepsValues(t *testing.T) {
	st := newCountingStore()
	st.binds["c1"] = "plain-uid"

	r := New(Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"plain-uid"} },
	})
	r.LoadFromStore()

	if k, ok := r.Resolve("c1"); !ok || k != "plain-uid" {
		t.Fatalf("c1 解析 = %q/%v, want plain-uid（未注入迁移函数时不应改值）", k, ok)
	}
}

// isAccountKey 只按 UUIDv4 形态区分新旧值：非该形态一律当旧值处理。
func TestIsAccountKey(t *testing.T) {
	cases := map[string]bool{
		"11111111-1111-4111-8111-111111111111": true,
		"uid-123":                              false,
		"":                                     false,
		"11111111111141118111111111111111":     false, // 无连字符
		"11111111-1111-4111-8111-11111111111":  false, // 长度不足
	}
	for in, want := range cases {
		if got := isAccountKey(in); got != want {
			t.Errorf("isAccountKey(%q) = %v, want %v", in, got, want)
		}
	}
}
