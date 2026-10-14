package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 入池账号自动获得主键；Status 透出主键，AuthByKey 能用它取回同一个账号。
func TestAddAssignsKeyAndAuthByKeyFindsIt(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1", Nickname: "n1"}
	p.Add(a)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("Status(u1) not found")
	}
	if st.Key == "" {
		t.Fatal("Status.Key is empty; pool should assign an AccountKey on Add")
	}
	got := p.AuthByKey(st.Key)
	if got == nil || got.UID != "u1" {
		t.Fatalf("AuthByKey(%q) = %+v, want uid=u1", st.Key, got)
	}
	if _, ok := p.StatusByKey(st.Key); !ok {
		t.Fatalf("StatusByKey(%q) not found", st.Key)
	}
}

func TestAuthByKeyUnknownReturnsNil(t *testing.T) {
	p := New("")
	if got := p.AuthByKey("no-such-key"); got != nil {
		t.Fatalf("AuthByKey(unknown) = %+v, want nil", got)
	}
	if _, ok := p.StatusByKey("no-such-key"); ok {
		t.Fatal("StatusByKey(unknown) should report not found")
	}
}

// 跨 provider 相同 uid 必须共存而不是互相覆盖 —— 这正是引入 AccountKey 的目的。
func TestSameUIDAcrossProvidersCoexist(t *testing.T) {
	p := New("")
	wb, err := auth.Parse([]byte(`{"provider":"workbuddy","auth":{"accessToken":"wb-at"},"account":{"uid":"dup","nickname":"wb"}}`))
	if err != nil {
		t.Fatalf("parse wb: %v", err)
	}
	tr, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"tr-at"},"account":{"uid":"dup","nickname":"trae"}}`))
	if err != nil {
		t.Fatalf("parse trae: %v", err)
	}
	p.Add(wb)
	p.Add(tr)

	if got := len(p.List()); got != 2 {
		t.Fatalf("List() len = %d, want 2 (same uid must not overwrite)", got)
	}
	k1, k2 := wb.Key(), tr.Key()
	if k1 == "" || k2 == "" || k1 == k2 {
		t.Fatalf("keys = %q / %q, want two distinct non-empty keys", k1, k2)
	}
	if got := p.AuthByKey(k1); got == nil || got.Nickname != "wb" {
		t.Fatalf("AuthByKey(k1) = %+v, want wb", got)
	}
	if got := p.AuthByKey(k2); got == nil || got.Nickname != "trae" {
		t.Fatalf("AuthByKey(k2) = %+v, want trae", got)
	}
}

// state.json 旧条目以 uid 为键：SyncToDir 拿到带主键的真实凭证后必须归位到 AccountKey，
// 且历史状态（credits 等）不能丢。
func TestSyncToDirMigratesUIDKeyedState(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":777}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	t.Cleanup(p.Close)

	a := &auth.Auth{UID: "u1", Nickname: "n1"}
	p.SyncToDir([]*auth.Auth{a})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("Status(u1) not found after SyncToDir")
	}
	if st.Credits != 777 {
		t.Fatalf("credits = %d, want 777 (state must survive key migration)", st.Credits)
	}
	if st.Key == "" {
		t.Fatal("Status.Key empty after SyncToDir")
	}
	// 旧 uid 键必须已从主索引移除，只剩 AccountKey 一条。
	if _, exists := p.byKey[auth.AccountKey("u1")]; exists {
		t.Fatal("legacy uid key still present in byKey after migration")
	}
	if got := p.AuthByKey(st.Key); got == nil || got.UID != "u1" {
		t.Fatalf("AuthByKey(%q) = %+v, want uid=u1", st.Key, got)
	}
}

// 写入侧必须按 AccountKey 作键，并把 uid 放进记录里；否则跨 provider 同 uid 会互相覆盖。
func TestStateSaveKeysByAccountKey(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	t.Cleanup(p.Close)

	a := &auth.Auth{UID: "u1", Nickname: "n1"}
	p.Add(a)
	p.SetCredits("u1", 321, 0)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			UID     string `json:"uid"`
			Credits int64  `json:"credits"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if len(sf.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(sf.Accounts))
	}
	key := string(a.Key())
	got, ok := sf.Accounts[key]
	if !ok {
		t.Fatalf("state is not keyed by AccountKey %q (accounts=%v)", key, func() []string {
			out := make([]string, 0, len(sf.Accounts))
			for k := range sf.Accounts {
				out = append(out, k)
			}
			return out
		}())
	}
	if got.UID != "u1" {
		t.Fatalf("state[%s].uid = %q, want u1", key, got.UID)
	}
	if got.Credits != 321 {
		t.Fatalf("state[%s].credits = %d, want 321", key, got.Credits)
	}
}

// 新格式 state（AccountKey 为键 + 记录内带 uid）必须能被加载、按主键命中，且 uid 正确。
func TestStateLoadRestoresAccountKeyKeyedState(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	const key = "11111111-2222-4333-8444-555555555555"
	body := `{"accounts":{"` + key + `":{"uid":"u1","credits":555}}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	t.Cleanup(p.Close)

	a, err := auth.Parse([]byte(`{"account_key":"` + key + `","auth":{"accessToken":"at"},"account":{"uid":"u1"}}`))
	if err != nil {
		t.Fatalf("parse auth: %v", err)
	}
	p.SyncToDir([]*auth.Auth{a})

	st, ok := p.StatusByKey(auth.AccountKey(key))
	if !ok {
		t.Fatal("StatusByKey not found after loading AccountKey-keyed state")
	}
	if st.UID != "u1" {
		t.Fatalf("restored uid = %q, want u1 (uid must come from the record, not the map key)", st.UID)
	}
	if st.Credits != 555 {
		t.Fatalf("restored credits = %d, want 555", st.Credits)
	}
	if got := p.AuthByKey(auth.AccountKey(key)); got == nil || got.UID != "u1" {
		t.Fatalf("AuthByKey(%s) = %+v, want uid=u1", key, got)
	}
}
