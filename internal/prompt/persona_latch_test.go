package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePersonaPair 在临时目录写一对同名 md/json，返回 md 路径。
func writePersonaPair(t *testing.T, jsonBody string) string {
	t.Helper()
	dir := t.TempDir()
	md := filepath.Join(dir, "coffee.md")
	if err := os.WriteFile(md, []byte("# coffee\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if jsonBody != "" {
		if err := os.WriteFile(filepath.Join(dir, "coffee.json"), []byte(jsonBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return md
}

// TestLatchAbsentFile 无同名 json → 门锁不存在，且不报错。
func TestLatchAbsentFile(t *testing.T) {
	md := writePersonaPair(t, "")
	l, err := LoadPersonaLatch(md)
	if err != nil {
		t.Fatalf("无 json 不应报错: %v", err)
	}
	if l != nil {
		t.Error("无 json 应返回 nil 门锁")
	}
}

// TestLatchNonMdPersona 非 .md 人格 → 不看 json，直接无门锁。
func TestLatchNonMdPersona(t *testing.T) {
	l, err := LoadPersonaLatch("some/persona.txt")
	if err != nil || l != nil {
		t.Errorf("非 md 应无门锁且无错, got l=%v err=%v", l, err)
	}
}

// TestLatchBuiltinName 内置名没有路径可依 → 无门锁。
func TestLatchBuiltinName(t *testing.T) {
	l, err := LoadPersonaLatch("haiou")
	if err != nil || l != nil {
		t.Errorf("内置名应无门锁且无错, got l=%v err=%v", l, err)
	}
}

// TestLatchMatchesWholeTrimmed 整句精确匹配：前后空白不算，多一个字不算。
func TestLatchMatchesWholeTrimmed(t *testing.T) {
	md := writePersonaPair(t, `{"latch":{"words":["冷咖啡","cold coffee"],"match":"whole_trimmed","reply":"R"}}`)
	l, err := LoadPersonaLatch(md)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"冷咖啡", true},
		{"  冷咖啡  ", true},     // 去首尾空白后相等
		{"冷咖啡\n", true},       // 换行也属空白
		{"cold coffee", true}, // 兼听词
		{"来杯冷咖啡", false},      // 多字不命中
		{"冷咖啡吗", false},
		{"Cold Coffee", false}, // 大小写敏感，与 gpt6-Astra 一致
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := l.MatchWholeTrimmed(c.in); got != c.want {
			t.Errorf("MatchWholeTrimmed(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

// TestLatchNilSafe nil 门锁的判定必须恒 false，不能 panic。
func TestLatchNilSafe(t *testing.T) {
	var l *Latch
	if l.MatchWholeTrimmed("冷咖啡") {
		t.Error("nil 门锁不应命中")
	}
}

// TestLatchReplyVerbatim 激活页正文逐字保留（含换行），不被 JSON 转义吞掉。
func TestLatchReplyVerbatim(t *testing.T) {
	want := "冷咖啡已激活\n\nQQ一群 1057540028\n"
	md := writePersonaPair(t, `{"latch":{"words":["冷咖啡"],"match":"whole_trimmed","reply":"`+
		"冷咖啡已激活\\n\\nQQ一群 1057540028\\n"+`"}}`)
	l, err := LoadPersonaLatch(md)
	if err != nil {
		t.Fatal(err)
	}
	if l.Reply != want {
		t.Errorf("reply 不逐字一致:\n got %q\nwant %q", l.Reply, want)
	}
}

// TestLatchBadConfig 配置损坏必须报错，不静默失效。
func TestLatchBadConfig(t *testing.T) {
	cases := map[string]string{
		"坏 JSON":    `{`,
		"空 reply":   `{"latch":{"words":["冷咖啡"],"match":"whole_trimmed","reply":"  "}}`,
		"空 words":   `{"latch":{"words":[],"match":"whole_trimmed","reply":"R"}}`,
		"未知 match":  `{"latch":{"words":["冷咖啡"],"match":"fuzzy","reply":"R"}}`,
		"非对象":       `[]`,
		"words 全空白": `{"latch":{"words":["  ",""],"match":"whole_trimmed","reply":"R"}}`,
	}
	for name, body := range cases {
		md := writePersonaPair(t, body)
		if _, err := LoadPersonaLatch(md); err == nil {
			t.Errorf("%s 应报错，got nil", name)
		}
	}
}

// TestLatchMatchOmitted match 字段省略时按 whole_trimmed 处理，不报错。
func TestLatchMatchOmitted(t *testing.T) {
	md := writePersonaPair(t, `{"latch":{"words":["冷咖啡"],"reply":"R"}}`)
	l, err := LoadPersonaLatch(md)
	if err != nil {
		t.Fatalf("省略 match 不应报错: %v", err)
	}
	if !l.MatchWholeTrimmed("冷咖啡") {
		t.Error("省略 match 应按 whole_trimmed 判定")
	}
}

// TestLatchWordsTrimmed words 里的空白被清掉，不影响命中。
func TestLatchWordsTrimmed(t *testing.T) {
	md := writePersonaPair(t, `{"latch":{"words":["  冷咖啡  "],"match":"whole_trimmed","reply":"R"}}`)
	l, err := LoadPersonaLatch(md)
	if err != nil {
		t.Fatal(err)
	}
	if !l.MatchWholeTrimmed("冷咖啡") {
		t.Error("words 中的多余空白应被清理")
	}
	if strings.TrimSpace(l.Reply) != "R" {
		t.Errorf("reply=%q", l.Reply)
	}
}
