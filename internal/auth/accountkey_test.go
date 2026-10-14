package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// isUUIDv4 校验 8-4-4-4-12 形态 + version 4 + RFC4122 variant。
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return s[14] == '4' && (s[19] == '8' || s[19] == '9' || s[19] == 'a' || s[19] == 'b' ||
		s[19] == 'A' || s[19] == 'B')
}

// NewAccountKey 产出合法 UUIDv4，且两次调用不重复。
func TestNewAccountKeyIsUUIDv4(t *testing.T) {
	k1, err := NewAccountKey()
	if err != nil {
		t.Fatalf("NewAccountKey: %v", err)
	}
	k2, err := NewAccountKey()
	if err != nil {
		t.Fatalf("NewAccountKey: %v", err)
	}
	if !isUUIDv4(string(k1)) {
		t.Fatalf("key %q is not a UUIDv4", k1)
	}
	if k1 == k2 {
		t.Fatalf("two generated keys collided: %q", k1)
	}
}

// 文件里显式写了 account_key 时以它为准。
func TestParseReadsAccountKey(t *testing.T) {
	const want = "11111111-2222-4333-8444-555555555555"
	raw := []byte(`{"account_key":"` + want + `","auth":{"accessToken":"at"},"account":{"uid":"u1"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := string(a.Key()); got != want {
		t.Fatalf("Key() = %q, want %q", got, want)
	}
}

// 旧文件没有 account_key：LoadDir 生成一个并落盘，且重载后保持不变（稳定性是关键）。
func TestLoadDirGeneratesStableAccountKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-u1.json")
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"at","refreshToken":"r"},"account":{"uid":"u1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("want 1 account, got %d", len(first))
	}
	key1 := string(first[0].Key())
	if !isUUIDv4(key1) {
		t.Fatalf("generated key %q is not a UUIDv4", key1)
	}

	// 落盘：文件里应带上同一个 key。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var probe struct {
		AccountKey string `json:"account_key"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.AccountKey != key1 {
		t.Fatalf("persisted account_key = %q, want %q", probe.AccountKey, key1)
	}

	// 重载：必须拿到同一个 key（否则 state.json / Redis 引用会失配）。
	second, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir reload: %v", err)
	}
	if len(second) != 1 || string(second[0].Key()) != key1 {
		t.Fatalf("reload key = %q, want %q", second[0].Key(), key1)
	}
}

// 不同账号必须拿到不同 key；已有 key 的账号不被改写。
func TestLoadDirAccountKeysAreUniqueAndPreserved(t *testing.T) {
	dir := t.TempDir()
	const existing = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	files := map[string]string{
		"workbuddy-u1.json": `{"account_key":"` + existing + `","auth":{"accessToken":"a1"},"account":{"uid":"u1"}}`,
		"trae-u2.json":      `{"auth":{"accessToken":"a2"},"account":{"uid":"u2"}}`,
		"workbuddy-u3.json": `{"auth":{"accessToken":"a3"},"account":{"uid":"u3"}}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 accounts, got %d", len(list))
	}
	keys := map[string]string{}
	for _, a := range list {
		keys[a.UID] = string(a.Key())
	}
	if keys["u1"] != existing {
		t.Fatalf("existing key was rewritten: got %q want %q", keys["u1"], existing)
	}
	for _, uid := range []string{"u1", "u2", "u3"} {
		if !isUUIDv4(keys[uid]) {
			t.Fatalf("uid=%s key=%q is not a UUIDv4", uid, keys[uid])
		}
	}
	if keys["u1"] == keys["u2"] || keys["u2"] == keys["u3"] || keys["u1"] == keys["u3"] {
		t.Fatalf("keys not unique: %+v", keys)
	}
}
