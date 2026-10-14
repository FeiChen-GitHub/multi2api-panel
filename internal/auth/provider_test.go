package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Provider 的零值语义：未指定 = WorkBuddy（旧凭证文件零回归）。
func TestProviderZeroValueIsWorkBuddy(t *testing.T) {
	a := &Auth{}
	if got := a.Provider(); got != ProviderWorkBuddy {
		t.Fatalf("Provider() = %q, want %q", got, ProviderWorkBuddy)
	}
}

// 文件里显式写了 provider 时以它为准。
func TestParseReadsProvider(t *testing.T) {
	raw := []byte(`{"provider":"trae","auth":{"accessToken":"at","refreshToken":"rt"},"account":{"uid":"u1"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := a.Provider(); got != ProviderTrae {
		t.Fatalf("Provider() = %q, want %q", got, ProviderTrae)
	}
}

// 扁平形同样支持 provider。
func TestParseFlatReadsProvider(t *testing.T) {
	raw := []byte(`{"provider":"trae","accessToken":"at","refreshToken":"rt","uid":"u1"}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := a.Provider(); got != ProviderTrae {
		t.Fatalf("Provider() = %q, want %q", got, ProviderTrae)
	}
}

// 没有 provider 字段的旧文件按 WorkBuddy 处理。
func TestParseLegacyFileDefaultsToWorkBuddy(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt"},"account":{"uid":"u1"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := a.Provider(); got != ProviderWorkBuddy {
		t.Fatalf("Provider() = %q, want %q", got, ProviderWorkBuddy)
	}
}

// 写回必须落盘 provider，否则重启后 provider 维度丢失。
func TestSaveAtomicWritesProvider(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "trae-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u1", FilePath: fp, provider: ProviderTrae}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var probe struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.Provider != string(ProviderTrae) {
		t.Fatalf("persisted provider = %q, want %q", probe.Provider, ProviderTrae)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.Provider() != ProviderTrae {
		t.Fatalf("roundtrip Provider() = %q, want %q", b.Provider(), ProviderTrae)
	}
}

// LoadDir 要同时加载 workbuddy*.json 与 trae*.json；内容无 provider 时按文件名前缀回落，
// 并把判定结果 backfill 回文件。
func TestLoadDirLoadsWorkBuddyAndTrae(t *testing.T) {
	dir := t.TempDir()
	wbPath := filepath.Join(dir, "workbuddy-u1.json")
	trPath := filepath.Join(dir, "trae-u2.json")
	if err := os.WriteFile(wbPath, []byte(`{"auth":{"accessToken":"a1","refreshToken":"r"},"account":{"uid":"u1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trPath, []byte(`{"auth":{"accessToken":"a2","refreshToken":"r"},"account":{"uid":"u2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	got := map[string]Provider{}
	for _, a := range list {
		got[a.UID] = a.Provider()
	}
	if got["u1"] != ProviderWorkBuddy {
		t.Fatalf("u1 provider = %q, want %q", got["u1"], ProviderWorkBuddy)
	}
	if got["u2"] != ProviderTrae {
		t.Fatalf("u2 provider = %q, want %q", got["u2"], ProviderTrae)
	}
	// backfill：两个文件都应被补上 provider 键并落盘。
	for _, tc := range []struct {
		path string
		want Provider
	}{{wbPath, ProviderWorkBuddy}, {trPath, ProviderTrae}} {
		raw, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		var probe struct {
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.path, err)
		}
		if probe.Provider != string(tc.want) {
			t.Fatalf("%s persisted provider = %q, want %q", filepath.Base(tc.path), probe.Provider, tc.want)
		}
	}
}
