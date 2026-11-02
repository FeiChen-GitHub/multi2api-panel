package session

import (
	"testing"
	"time"
)

func TestLastUserMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"last user wins", `{"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`, "第二问"},
		{"single", `{"messages":[{"role":"user","content":"你好"}]}`, "你好"},
		{"multimodal joins", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"x"}}]}]}`, "看图"},
		{"no user", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"responses input", `{"input":[{"role":"assistant","content":"a"},{"role":"user","content":"responses 问"}]}`, "responses 问"},
		{"broken", `{broken`, ""},
		{"env context only -> empty", `{"messages":[{"role":"user","content":"<environment_context> <cwd>C:/x</cwd>"}]}`, ""},
		{"env then real -> real", `{"messages":[{"role":"user","content":"<environment_context> <cwd>C:/x</cwd>"},{"role":"user","content":"真问题"}]}`, "真问题"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LastUserMessage([]byte(c.body)); got != c.want {
				t.Errorf("LastUserMessage = %q want %q", got, c.want)
			}
		})
	}
}

func TestUserMessageCache(t *testing.T) {
	c := NewUserMessageCache(2, time.Minute)
	if _, ok := c.Get("trae|s1"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set("trae|s1", "问题一")
	if got, ok := c.Get("trae|s1"); !ok || got != "问题一" {
		t.Fatalf("get = %q,%v want 问题一,true", got, ok)
	}
	// 覆盖更新
	c.Set("trae|s1", "问题二")
	if got, _ := c.Get("trae|s1"); got != "问题二" {
		t.Fatalf("after update get = %q", got)
	}
	// 空 key 忽略
	c.Set("", "x")
	c.Set("trae|s2", "")
	if _, ok := c.Get("trae|s2"); ok {
		t.Fatal("empty text should not be stored")
	}
	// 满上限后仍能写入（淘汰策略），但保留的是有的条目
	c.Set("trae|s3", "第三")
	c.Set("trae|s4", "第四")
	if len(c.m) == 0 {
		t.Fatal("cache emptied unexpectedly")
	}
}

func TestUserMessageCacheTTL(t *testing.T) {
	c := NewUserMessageCache(4, 10*time.Millisecond)
	c.Set("trae|s1", "x")
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get("trae|s1"); ok {
		t.Fatal("expired entry should miss")
	}
}

