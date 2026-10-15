package main

import (
	"strings"
	"testing"
)

// provider 开关语义：未配置或 enabled 缺省 = 启用；显式 false 才关闭。
func TestProviderEnabledDefaultsAndOverride(t *testing.T) {
	c := &Config{}
	if !c.ProviderEnabled("trae") {
		t.Fatal("未配置的 provider 应默认启用")
	}
	off := false
	c.Upstream.Providers = map[string]ProviderConfig{"trae": {Enabled: &off}}
	if c.ProviderEnabled("trae") {
		t.Fatal("enabled=false 应关闭 provider")
	}
	on := true
	c.Upstream.Providers["trae"] = ProviderConfig{Enabled: &on}
	if !c.ProviderEnabled("trae") {
		t.Fatal("enabled=true 应启用 provider")
	}
}

// providers 的键必须是已实现的 provider（DESIGN §8）：拼错时旧行为是静默忽略，
// 整块配置失效且用户看不出；现在 fail fast。
func TestProviderNameValidated(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("已知/缺省 providers 不应报错: %v", err)
	}

	c.Upstream.Providers = map[string]ProviderConfig{"workbudddy": {}}
	err := c.normalize()
	if err == nil {
		t.Fatal("拼错的 provider 名应报错")
	}
	if !strings.Contains(err.Error(), "workbudddy") {
		t.Fatalf("错误信息应点出拼错的键，got %v", err)
	}

	c.Upstream.Providers = map[string]ProviderConfig{"workbuddy": {}, "trae": {}}
	if err := c.normalize(); err != nil {
		t.Fatalf("两个已实现 provider 都应通过: %v", err)
	}
}
