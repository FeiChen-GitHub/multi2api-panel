package upstream

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestTraeFetchModelsReadsSoloAgentFromStateVSCDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatalf("create ItemTable: %v", err)
	}
	payload := `{"solo_agent":[` +
		`{"name":"cached-model","display_name":"Cached Model","is_default":true,"multimodal":true,` +
		`"model_type":"reasoning_model","context_window_size":{"default":123000,"max":[456000]},` +
		`"max_tokens":32000,"features":{"consumption_rate":{"data":{"rate":0.42}}},` +
		`"reasoning_effort_config":{"support_thinking":true,"options":["light","high"],"default_level":"high"}},` +
		`{"name":"disabled-model","status":false,"selectable":false}]}`
	if _, err := db.Exec(`INSERT INTO ItemTable(key,value) VALUES(?,?)`, "u1:AI.agent.model.model_list_map", payload); err != nil {
		t.Fatalf("insert cache: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	c := NewTraeClient()
	c.ModelCachePath = dbPath
	infos, err := c.FetchModels(context.Background(), &auth.Auth{UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("models = %d, want cached-model + Auto: %+v", len(infos), infos)
	}
	got := infos[0]
	if got.ID != "cached-model" || got.Name != "Cached Model" || !got.IsDefault || !got.SupportsImages || !got.SupportsReasoning {
		t.Fatalf("cached model fields = %+v", got)
	}
	if got.ContextWindow != 123000 || got.MaxTokens != 32000 || got.Credits != "x0.42" {
		t.Fatalf("cached model limits/credits = %+v", got)
	}
	if len(got.Efforts) != 2 || got.Efforts[0] != "light" || got.DefaultEffort != "high" {
		t.Fatalf("cached model efforts = %+v", got)
	}
	if infos[1].ID != "Auto" {
		t.Fatalf("last model = %+v, want Auto", infos[1])
	}
}

func TestTraeFetchModelsFallsBackWhenStateVSCDBMissing(t *testing.T) {
	c := NewTraeClient()
	c.ModelCachePath = filepath.Join(t.TempDir(), "missing.vscdb")
	infos, err := c.FetchModels(context.Background(), &auth.Auth{UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(infos) != 22 {
		t.Fatalf("fallback models = %d, want 21 static + Auto", len(infos))
	}
	if infos[21].ID != "Auto" {
		t.Fatalf("last fallback model = %+v, want Auto", infos[21])
	}
}
