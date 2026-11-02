package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 粘性会话 tab 必须出现在导航与视图里，且接口/脚本已接线。
func TestStickyTabAndViewWired(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`data-view="sticky"`,     // 导航项
		`id="view-sticky"`,       // 视图容器
		`id="stickyBody"`,        // 绑定表格
		`id="stickyAcct"`,        // 账号筛选
		`id="btnStickyUnbind"`,   // 解绑选中
		`id="btnStickyUnbindAll"`, // 解绑全部
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing %q", want)
		}
	}

	// 脚本侧：loadSticky / renderSticky / 事件接线存在
	js := rec2String(t, "/panel/app.js")
	for _, want := range []string{
		"function loadSticky", "function renderSticky",
		"sticky/unbind", "stickySel", "btnStickyReload",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}

func rec2String(t *testing.T, path string) string {
	t.Helper()
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Body.String()
}

