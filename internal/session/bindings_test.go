package session

import (
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Bindings 必须枚举全部有效绑定、按会话键排序，且与 Count 一致。
func TestBindingsSnapshotSortedAndConsistent(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a", "b"}, time.Minute)
	r.Bind("sess-2", "a")
	r.Bind("sess-1", "b")
	r.Bind("sess-3", "a")

	got := r.Bindings()
	if len(got) != 3 {
		t.Fatalf("Bindings len=%d want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].SessionKey >= got[i].SessionKey {
			t.Fatalf("Bindings not sorted by SessionKey: %q before %q", got[i-1].SessionKey, got[i].SessionKey)
		}
	}
	// 绑定值回读
	want := map[string]auth.AccountKey{"sess-1": "b", "sess-2": "a", "sess-3": "a"}
	for _, b := range got {
		if want[b.SessionKey] != b.AccountKey {
			t.Errorf("binding %q = %q want %q", b.SessionKey, b.AccountKey, want[b.SessionKey])
		}
	}
	if r.Count() != 3 {
		t.Fatalf("Count=%d want 3", r.Count())
	}
}

// UnbindMany 只解绑存在的会话键，返回实际解绑条数，其余绑定不受影响。
func TestUnbindManyPartial(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a", "b"}, time.Minute)
	r.Bind("sess-1", "a")
	r.Bind("sess-2", "b")
	r.Bind("sess-3", "a")

	n := r.UnbindMany([]string{"sess-1", "sess-2", "not-exist"})
	if n != 2 {
		t.Fatalf("UnbindMany n=%d want 2", n)
	}
	keys := make([]string, 0)
	for _, b := range r.Bindings() {
		keys = append(keys, b.SessionKey)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "sess-3" {
		t.Fatalf("remaining bindings = %v want [sess-3]", keys)
	}
	// Redis 镜像同步删除
	if _, ok := st.binds["sess-1"]; ok {
		t.Error("sess-1 bind still mirrored")
	}
	if _, ok := st.binds["sess-2"]; ok {
		t.Error("sess-2 bind still mirrored")
	}
}
