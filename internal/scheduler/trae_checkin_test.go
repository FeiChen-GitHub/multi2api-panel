package scheduler

import (
	"context"
	"io"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeChatClient 记录 ChatClient 调用，用于验证 provider 分发。
type fakeChatClient struct {
	checkinCalls int
	creditsCalls int
	remain       int64
	total        int64
}

func (f *fakeChatClient) ChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta upstream.ChatMeta, ch upstream.Channel) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (f *fakeChatClient) Checkin(ctx context.Context, a *auth.Auth) error      { f.checkinCalls++; return nil }
func (f *fakeChatClient) RefreshToken(ctx context.Context, a *auth.Auth) error { return nil }
func (f *fakeChatClient) QueryCredits(ctx context.Context, a *auth.Auth) (int64, int64, error) {
	f.creditsCalls++
	return f.remain, f.total, nil
}

// 「全部签到」（RunCheckinNow）必须覆盖 Trae 账号：走 ChatClient.Checkin +
// QueryCredits，且不碰 WorkBuddy 客户端（DESIGN §11.1）。
func TestRunCheckinNowCoversTraeAccounts(t *testing.T) {
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999},"account":{"uid":"trae1"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := pool.New("")
	p.Add(traeAuth)

	wb := &fakeChatClient{}
	tr := &fakeChatClient{remain: 321, total: 500}
	s := New(Config{
		Pool:       p,
		Upstream:   &upstream.Client{},
		Dispatcher: upstream.NewDispatcher(wb, tr),
	})
	s.RunCheckinNow()

	if tr.checkinCalls != 1 {
		t.Fatalf("trae Checkin calls = %d, want 1", tr.checkinCalls)
	}
	if tr.creditsCalls != 1 {
		t.Fatalf("trae QueryCredits calls = %d, want 1", tr.creditsCalls)
	}
	if wb.checkinCalls != 0 {
		t.Fatalf("workbuddy Checkin calls = %d, want 0（Trae 账号不得打到 WorkBuddy）", wb.checkinCalls)
	}
	st, ok := p.Status("trae1")
	if !ok {
		t.Fatal("trae1 not in pool")
	}
	if st.Credits != 321 {
		t.Fatalf("credits = %d, want 321 (from QueryCredits)", st.Credits)
	}
}

// 全局（global）realm 的 Trae 账号不签到（phase 1 只有 cn）。
func TestRunCheckinSkipsTraeGlobal(t *testing.T) {
	traeAuth, err := auth.Parse([]byte(`{"provider":"trae","auth":{"accessToken":"trae-at","expiresAt":9999999999,"realm":"global"},"account":{"uid":"trae-g"}}`))
	if err != nil {
		t.Fatalf("parse trae auth: %v", err)
	}
	p := pool.New("")
	p.Add(traeAuth)

	tr := &fakeChatClient{}
	s := New(Config{Pool: p, Upstream: &upstream.Client{}, Dispatcher: upstream.NewDispatcher(&fakeChatClient{}, tr)})
	s.RunCheckinNow()

	if tr.checkinCalls != 0 {
		t.Fatalf("trae global Checkin calls = %d, want 0", tr.checkinCalls)
	}
}
