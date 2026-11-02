package panel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// 聚合：同一 SessionKey 的多条请求 → 取最近一条的标题/模型/结果/来源，token 累加。
func TestStickyAggregateFromArchive(t *testing.T) {
	dir := t.TempDir()
	r := reqlog.New(reqlog.Config{Enabled: true, Dir: dir, RetentionDays: 7})
	base := time.Now().Add(-time.Hour)
	// 两个会话：s1 两条（最新一条标题/模型覆盖旧值，token 累加）；s2 一条；无键一条不参与。
	r.Record(reqlog.Event{Time: base, RequestID: "r1", SessionKey: "trae|s1", Title: "旧标题", Model: "glm-5.2",
		Status: 200, Outcome: reqlog.OutcomeSuccess, TotalTokens: 100, ClientIP: "1.1.1.1", UserAgent: "ua-old"})
	r.Record(reqlog.Event{Time: base.Add(time.Minute), RequestID: "r2", SessionKey: "trae|s1", Title: "新标题", Model: "deepseek-v4.1-flash",
		Status: 429, Outcome: reqlog.OutcomeHTTPError, TotalTokens: 50, ClientIP: "2.2.2.2", UserAgent: "ua-new"})
	r.Record(reqlog.Event{Time: base.Add(2 * time.Minute), RequestID: "r3", SessionKey: "wb|s2", Title: "会话二", Model: "hy3",
		Status: 200, Outcome: reqlog.OutcomeSuccess, TotalTokens: 30})
	r.Record(reqlog.Event{Time: base.Add(3 * time.Minute), RequestID: "r4", SessionKey: "", Title: "无键", Model: "x", TotalTokens: 99})
	// 归档写入是异步队列 + 1s ticker flush：等待落盘后再读。
	time.Sleep(1300 * time.Millisecond)
	defer r.Close()

	p := New(Config{
		Version: "test", APIKey: "k", RequestLog: r,
		StickySessions: func() []StickySession {
			return []StickySession{
				{SessionKey: "trae|s1", AccountKey: "a", LastActive: time.Now()},
				{SessionKey: "wb|s2", AccountKey: "b", LastActive: time.Now()},
			}
		},
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq("GET", "/panel/api/sticky"))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var ld struct {
		Entries []StickySession `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ld); err != nil {
		t.Fatal(err)
	}
	if len(ld.Entries) != 2 {
		t.Fatalf("entries=%d want 2", len(ld.Entries))
	}
	byKey := map[string]StickySession{}
	for _, e := range ld.Entries {
		byKey[e.SessionKey] = e
	}
	s1, ok := byKey["trae|s1"]
	if !ok {
		t.Fatal("trae|s1 missing")
	}
	if s1.Title != "新标题" || s1.Model != "deepseek-v4.1-flash" || s1.Outcome != reqlog.OutcomeHTTPError {
		t.Fatalf("s1 latest fields = %+v", s1)
	}
	if s1.TotalTokens != 150 {
		t.Fatalf("s1 total tokens = %d want 150", s1.TotalTokens)
	}
	if s1.ClientIP != "2.2.2.2" || s1.UserAgent != "ua-new" {
		t.Fatalf("s1 source = %+v", s1)
	}
	s2 := byKey["wb|s2"]
	if s2.Title != "会话二" || s2.TotalTokens != 30 {
		t.Fatalf("s2 = %+v", s2)
	}
}

