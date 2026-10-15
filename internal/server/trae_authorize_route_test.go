package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePanelWithAuthorize 模拟面板：只实现主路由需要的「Trae 回调 handler」能力。
// server 不 import panel（避免循环依赖），挂载靠这个隐式接口，测试也照此构造。
type fakePanelWithAuthorize struct {
	hit bool
}

func (f *fakePanelWithAuthorize) TraeAuthorizeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func (f *fakePanelWithAuthorize) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

// Trae 登录回调是浏览器 302 跳转，带不上 Bearer：/authorize 必须挂在根 mux 且免鉴权，
// 否则整条网页登录链路在回调这一步就断（用户看到 401，服务端拿不到凭证）。
func TestTraeAuthorizeMountedWithoutAuth(t *testing.T) {
	fp := &fakePanelWithAuthorize{}
	h := NewHandler(Config{Panel: fp, APIKey: "k"})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/authorize?refreshToken=rt", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s, want 200（/authorize 必须免鉴权可达）", rr.Code, rr.Body.String())
	}
	if !fp.hit {
		t.Fatal("请求未落到面板的回调 handler：/authorize 没挂上根 mux")
	}
}

// 未挂面板时不应留下半吊子路由（旧行为：未匹配 → 404）。
func TestTraeAuthorizeAbsentWithoutPanel(t *testing.T) {
	h := NewHandler(Config{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/authorize", nil))
	if rr.Code != 404 {
		t.Fatalf("code=%d, want 404（未挂载面板时不应注册回调路由）", rr.Code)
	}
}
