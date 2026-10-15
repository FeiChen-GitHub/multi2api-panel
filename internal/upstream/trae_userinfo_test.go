package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// GetUserInfo 必须打 OAuth host 的 GetUserInfo，带 Cloud-IDE-JWT，并拆出 Result 三字段。
func TestTraeGetUserInfo(t *testing.T) {
	var gotPath, gotToken, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Cloudide-Token")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"Result":{"UserID":"u-7","ScreenName":"七号","EnterpriseID":"ent-7"}}`))
	}))
	defer srv.Close()

	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "jwt-7"}
	uid, nick, ent, err := c.GetUserInfo(context.Background(), a)
	if err != nil {
		t.Fatalf("GetUserInfo: %v", err)
	}
	if gotPath != TraeEpUserInfo {
		t.Fatalf("path = %q, want %q", gotPath, TraeEpUserInfo)
	}
	if gotToken != "jwt-7" {
		t.Fatalf("X-Cloudide-Token = %q, want jwt-7", gotToken)
	}
	if gotBody == "" {
		t.Fatal("GetUserInfo 未带请求体")
	}
	if uid != "u-7" || nick != "七号" || ent != "ent-7" {
		t.Fatalf("uid/nick/ent = %q/%q/%q", uid, nick, ent)
	}
}

// 上游非 2xx 必须报带错误类别的错（登录据此判 failed，而不是拿空 uid 落盘）。
func TestTraeGetUserInfoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"msg":"unauthorized"}`))
	}))
	defer srv.Close()

	c := newTestTraeClient(srv)
	_, _, _, err := c.GetUserInfo(context.Background(), &auth.Auth{AccessToken: "bad"})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrSessionDead {
		t.Fatalf("err = %v, want ErrSessionDead", err)
	}
}
