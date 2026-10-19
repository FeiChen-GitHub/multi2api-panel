package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 上游对失效凭证也回 HTTP 200（code=1001、enable=false），Checkin 不能把它
// 当成「签到未开放」的幂等成功，必须报 ErrSessionDead。
func TestTraeCheckinUnauthorized200IsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TraeEpCheckinStatus {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"checked_in":false,"code":1001,"enable":false,"message":"not able to authenticate you"}`))
	}))
	defer srv.Close()
	c := newTestTraeClient(srv)
	a := &auth.Auth{AccessToken: "bogus-at", UID: "u1"}
	err := c.Checkin(context.Background(), a)
	if err == nil {
		t.Fatal("Checkin succeeded with invalid token, want ErrSessionDead")
	}
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type = %T, want *Error (%v)", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Fatalf("err kind = %s, want %s", ue.Kind, ErrSessionDead)
	}
}
