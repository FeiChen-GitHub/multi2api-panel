package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Trae 凭证的三个专属字段：apiHost（ExchangeToken host）、machineId / deviceId（出站头）。
// 字段名对照 trae2api/internal/auth/auth.go，落盘位置是嵌套形的 auth 对象内。
func TestParseReadsTraeFields(t *testing.T) {
	raw := []byte(`{"provider":"trae","auth":{"accessToken":"at","refreshToken":"rt",` +
		`"apiHost":"https://api.trae.com.cn","machineId":"m-1","deviceId":"1234567890123456"},` +
		`"account":{"uid":"u1"}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.ApiHost != "https://api.trae.com.cn" {
		t.Fatalf("ApiHost = %q", a.ApiHost)
	}
	if a.MachineID != "m-1" {
		t.Fatalf("MachineID = %q", a.MachineID)
	}
	if a.DeviceID != "1234567890123456" {
		t.Fatalf("DeviceID = %q", a.DeviceID)
	}
}

// 写回必须保留这三个字段，否则 refresh 后 Trae 出站会缺头（缺 x-device-id 签到直接 9004）。
func TestSaveAtomicRoundTripsTraeFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "trae-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u1", FilePath: fp,
		provider: ProviderTrae, ApiHost: "https://api.trae.com.cn",
		MachineID: "m-1", DeviceID: "1234567890123456"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 落盘位置：嵌套形的 auth 对象内（与 trae2api 输出一致）。
	var probe struct {
		Auth struct {
			MachineID string `json:"machineId"`
			DeviceID  string `json:"deviceId"`
			ApiHost   string `json:"apiHost"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.Auth.MachineID != "m-1" || probe.Auth.DeviceID != "1234567890123456" || probe.Auth.ApiHost != "https://api.trae.com.cn" {
		t.Fatalf("persisted trae fields = %+v", probe.Auth)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.MachineID != "m-1" || b.DeviceID != "1234567890123456" || b.ApiHost != "https://api.trae.com.cn" {
		t.Fatalf("roundtrip trae fields = %+v", b)
	}
}
