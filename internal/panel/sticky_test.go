package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 未注入 StickySessions / UnbindStickySessions（粘性路由关闭/纯测试面板）时，
// list 与 unbind 都应返回 501，前端据此隐藏入口。
func TestStickyEndpointsNotAvailable(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, authReq("GET", "/panel/api/sticky"))
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("list code=%d body=%s, want 501", rr.Code, rr.Body.String())
	}
	rr2 := httptest.NewRecorder()
	p.ServeHTTP(rr2, authReq("POST", "/panel/api/sticky/unbind", `{"keys":["trae|s1"]}`))
	if rr2.Code != http.StatusNotImplemented {
		t.Fatalf("unbind code=%d body=%s, want 501", rr2.Code, rr2.Body.String())
	}
}

// 注入后：list 返回全部绑定（含账号展示字段），unbind 按会话键批量解绑并返回条数。
func TestStickyListAndUnbind(t *testing.T) {
	var store = map[string]string{
		"trae|conv-1": "acct-a",
		"trae|d-hex1": "acct-b",
		"wb|conv-2":   "acct-a",
	}
	listFn := func() []StickySession {
		out := []StickySession{}
		for k, v := range store {
			ss := StickySession{
				SessionKey: k,
				AccountKey: v,
				LastActive: time.Now().Add(-time.Minute),
			}
			if v == "acct-a" {
				ss.UID = "uid-1"
				ss.Nickname = "甲号"
				ss.Provider = "trae"
				ss.Realm = "cn"
			} else {
				ss.UID = "uid-2"
				ss.Nickname = "乙号"
				ss.Provider = "trae"
				ss.Realm = "cn"
			}
			out = append(out, ss)
		}
		return out
	}
	unbindFn := func(keys []string) (int, error) {
		n := 0
		for _, k := range keys {
			if _, ok := store[k]; ok {
				delete(store, k)
				n++
			}
		}
		return n, nil
	}
	p := New(Config{Version: "test", APIKey: "k", StickySessions: listFn, UnbindStickySessions: unbindFn})

	// list
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, authReq("GET", "/panel/api/sticky"))
	if rr.Code != 200 {
		t.Fatalf("list code=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var ld struct {
		Entries []StickySession `json:"entries"`
		Count   int             `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ld); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if ld.Count != 3 || len(ld.Entries) != 3 {
		t.Fatalf("count=%d entries=%d want 3", ld.Count, len(ld.Entries))
	}
	// 账号展示字段透出（nickname/uid/provider）
	var sawNick bool
	for _, e := range ld.Entries {
		if e.Nickname != "" && e.UID != "" {
			sawNick = true
		}
	}
	if !sawNick {
		t.Fatal("list entries missing account display fields")
	}

	// unbind 两个会话键
	body, _ := json.Marshal(map[string]any{"keys": []string{"trae|conv-1", "trae|d-hex1", "not-exist"}})
	rr2 := httptest.NewRecorder()
	p.ServeHTTP(rr2, authReq("POST", "/panel/api/sticky/unbind", string(body)))
	if rr2.Code != 200 {
		t.Fatalf("unbind code=%d body=%s, want 200", rr2.Code, rr2.Body.String())
	}
	var ud struct {
		OK      bool `json:"ok"`
		Unbound int  `json:"unbound"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &ud); err != nil {
		t.Fatalf("unbind decode: %v", err)
	}
	if !ud.OK || ud.Unbound != 2 {
		t.Fatalf("ok=%v unbound=%d want 2", ud.OK, ud.Unbound)
	}
	if _, ok := store["wb|conv-2"]; !ok {
		t.Fatal("wb|conv-2 should remain bound")
	}
}

// 防御：空键/去重后无有效键 → 直接 200 unbound=0，不调用底层。
func TestStickyUnbindDedupAndEmpty(t *testing.T) {
	called := false
	p := New(Config{
		Version: "test", APIKey: "k",
		UnbindStickySessions: func(keys []string) (int, error) { called = true; return len(keys), nil },
	})
	body := `{"keys":["", "   ", ""]}`
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, authReq("POST", "/panel/api/sticky/unbind", body))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("unbind should not be called for empty/deduped key set")
	}
	if !strings.Contains(rr.Body.String(), `"unbound":0`) {
		t.Fatalf("body=%s, want unbound:0", rr.Body.String())
	}
}


