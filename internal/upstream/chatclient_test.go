package upstream

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// fakeClient 记录身份，用来验证 Dispatcher 路由。
type fakeClient struct{ name string }

func (f *fakeClient) ChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta, ch Channel) (io.ReadCloser, int, []byte, error) {
	return nil, 200, []byte(f.name), nil
}
func (f *fakeClient) Checkin(ctx context.Context, a *auth.Auth) error      { return nil }
func (f *fakeClient) RefreshToken(ctx context.Context, a *auth.Auth) error { return nil }
func (f *fakeClient) QueryCredits(ctx context.Context, a *auth.Auth) (int64, int64, error) {
	return 0, 0, nil
}
func (f *fakeClient) AccountCredits(ctx context.Context, a *auth.Auth, soon time.Duration) (int64, int64, int64, time.Time, int64, error) {
	return 0, 0, 0, time.Time{}, 0, nil
}
func (f *fakeClient) CreditPackages(ctx context.Context, a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	return nil, 0, 0, nil
}

// 现有 WorkBuddy 客户端经适配后必须满足 ChatClient。
var _ ChatClient = (*WBClient)(nil)

// Dispatcher 按账号 provider 选客户端；未知 provider 回落 WorkBuddy（路由层已保证合法）。
func TestDispatcherRoutesByProvider(t *testing.T) {
	wb := &fakeClient{name: "wb"}
	tr := &fakeClient{name: "trae"}
	d := NewDispatcher(wb, tr)

	if got := d.For(auth.ProviderWorkBuddy); got != ChatClient(wb) {
		t.Fatalf("For(workbuddy) = %#v, want wb client", got)
	}
	if got := d.For(auth.ProviderTrae); got != ChatClient(tr) {
		t.Fatalf("For(trae) = %#v, want trae client", got)
	}
	if got := d.For(auth.Provider("unknown")); got != ChatClient(wb) {
		t.Fatalf("For(unknown) = %#v, want wb fallback", got)
	}
}

// Trae 未接入时（nil）必须返回 nil，而不是把请求误发给 WorkBuddy。
func TestDispatcherNilTraeReturnsNil(t *testing.T) {
	d := NewDispatcher(&fakeClient{name: "wb"}, nil)
	if got := d.For(auth.ProviderTrae); got != nil {
		t.Fatalf("For(trae) with nil trae client = %#v, want nil", got)
	}
}
