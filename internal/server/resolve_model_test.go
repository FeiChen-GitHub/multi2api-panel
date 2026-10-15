package server

import (
	"errors"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 路由协议表（DESIGN §3.1）：provider 必填，realm 缺省 cn。
func TestResolveModelTable(t *testing.T) {
	cases := []struct {
		in       string
		provider auth.Provider
		realm    string
		bare     string
	}{
		{"wb:glm-5.3", auth.ProviderWorkBuddy, "cn", "glm-5.3"},
		{"wb:cn:auto", auth.ProviderWorkBuddy, "cn", "auto"},
		{"workbuddy:glm-5.3", auth.ProviderWorkBuddy, "cn", "glm-5.3"},
		{"wb:global:xxx", auth.ProviderWorkBuddy, "global", "xxx"},
		{"trae:glm-5.3", auth.ProviderTrae, "cn", "glm-5.3"},
		{"trae:cn:auto", auth.ProviderTrae, "cn", "auto"},
		{"trae:global:xxx", auth.ProviderTrae, "global", "xxx"},
	}
	for _, tc := range cases {
		p, realm, bare, err := resolveModel(tc.in)
		if err != nil {
			t.Errorf("%s: unexpected err %v", tc.in, err)
			continue
		}
		if p != tc.provider || realm != tc.realm || bare != tc.bare {
			t.Errorf("%s → (%s,%s,%s), want (%s,%s,%s)", tc.in, p, realm, bare, tc.provider, tc.realm, tc.bare)
		}
	}
}

// provider 必填：缺 provider / 未知 provider / 空模型名都要报 ErrProviderRequired。
func TestResolveModelRequiresProvider(t *testing.T) {
	for _, in := range []string{
		"glm-5.3",         // 裸名
		"cn:glm-5.3",      // 旧 realm 前缀（无 provider）
		"global:trae:xxx", // 段序错误：第一段不是 provider
		"foo:bar",         // 未知 provider
		"wb:",             // 空模型名
		"trae:cn:",        // 空模型名
	} {
		if _, _, _, err := resolveModel(in); !errors.Is(err, ErrProviderRequired) {
			t.Errorf("%s: err = %v, want ErrProviderRequired", in, err)
		}
	}
}
