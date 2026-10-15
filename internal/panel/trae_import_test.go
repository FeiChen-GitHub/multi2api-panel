package panel

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Trae 凭证导入：落盘为 trae-<uid>.json、标记 provider=trae、realm=cn，并热加载进池。
func TestImportTraeWritesProviderFile(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "k", AuthDir: dir, Pool: pool.New(""), Upstream: &upstream.Client{}})

	body := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":9999999999,` +
		`"apiHost":"https://api.trae.com.cn","machineId":"m-1","deviceId":"1234567890123456"},` +
		`"account":{"uid":"trae-u1","nickname":"T"}}`
	req := httptest.NewRequest("POST", "/panel/api/import/trae", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "trae-trae-u1.json"))
	if err != nil {
		t.Fatalf("expected trae-<uid>.json on disk: %v", err)
	}
	back, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if back.Provider() != auth.ProviderTrae {
		t.Fatalf("persisted provider = %q, want trae", back.Provider())
	}
	if back.Realm() != "cn" {
		t.Fatalf("persisted realm = %q, want cn", back.Realm())
	}
	if back.ApiHost != "https://api.trae.com.cn" || back.MachineID != "m-1" || back.DeviceID != "1234567890123456" {
		t.Fatalf("trae fields lost: %+v", back)
	}

	st, ok := p.cfg.Pool.Status("trae-u1")
	if !ok {
		t.Fatal("imported account not in pool")
	}
	if st.Provider != auth.ProviderTrae {
		t.Fatalf("pool provider = %q, want trae", st.Provider)
	}
}

// UID 含路径字符必须被拒（防穿越），且不落盘。
func TestImportTraeRejectsUnsafeUID(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "k", AuthDir: dir, Pool: pool.New(""), Upstream: &upstream.Client{}})

	body := `{"auth":{"accessToken":"at"},"account":{"uid":"../evil"}}`
	req := httptest.NewRequest("POST", "/panel/api/import/trae", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("no file should be written for unsafe uid, got %d entries", len(entries))
	}
}
