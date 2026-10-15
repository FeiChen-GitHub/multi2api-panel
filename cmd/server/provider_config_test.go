package main

import "testing"

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
