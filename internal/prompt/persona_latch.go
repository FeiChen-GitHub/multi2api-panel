// persona_latch.go 人格门锁（activation latch）：与人格 md 同目录同名的 .json。
//
// 为什么门锁不在提示词里：激活词写进 md 意味着每个请求都把激活页正文（约 1KB）
// 塞进 system，而它只在极少数请求里用得上。更关键的是——网关本身就能看到
// 用户消息，精确匹配后直接短路返回，连上游都不用打。提示词里的「某某词则输出
// 某某页」是让模型去模仿一段文本，逐字一致根本无法保证；代码层返回则是字节级
// 确定的。参照 gpt6-Astra 的 is_activation_latch，但判定放在网关而不是提示词。
//
// 有则用，无则无：同名 json 不存在 → 门锁整关不存在，人格的其它部分照常工作。
// 文件损坏（JSON 解析失败 / 缺 reply）→ 启动报错，不静默失效。
package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Latch 一份人格的门锁配置。
type Latch struct {
	Words []string // 激活词，整句精确匹配
	Reply string   // 命中时的完整回复正文，逐字返回
}

// MatchWholeTrimmed 判定用户输入是否命中门锁。
//
// 口径与 gpt6-Astra 的 is_activation_latch 一致：去首尾空白后整句相等。
// 「来杯冷咖啡」不命中——不是保守，是原实现就这么定，多放一个词就丢了激活语义。
func (l *Latch) MatchWholeTrimmed(input string) bool {
	if l == nil {
		return false
	}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return false
	}
	for _, w := range l.Words {
		if trimmed == w {
			return true
		}
	}
	return false
}

// latchFile 人格文件 → 同目录同名 .json。
//
// 先按与 LoadPersona 完全相同的规则解析路径（cwd 优先、exe 目录兜底），
// 否则会出现"md 靠 exe 兜底找到了、json 却按 cwd 找不到 → 门锁静默失效"。
// 仅对 .md 生效（模板/其它后缀的人格不入此门），后缀替换而非拼接：
// personas/coffee.md → personas/coffee.json，不产生 coffee.md.json。
func latchFile(personaFile string) string {
	resolved := resolvePersonaPath(personaFile)
	if filepath.Ext(resolved) != ".md" {
		return ""
	}
	return strings.TrimSuffix(resolved, ".md") + ".json"
}

// LoadPersonaLatch 加载人格文件对应的门锁。
//
// 返回值 nil 表示该人格没有门锁（不是错误）：
//   - persona 是内置名（无路径可得）→ 无门锁
//   - 文件不是 .md → 无门锁
//   - 同名 .json 不存在 → 无门锁
//
// 除「不存在」以外的一切失败都返回 error，交调用方 fail fast：
// 门锁是安全语义，静默失效会让用户以为激活词被拦住了，实际直接进了路由。
func LoadPersonaLatch(personaFile string) (*Latch, error) {
	path := latchFile(personaFile)
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("persona latch %s: %w", path, err)
	}
	var doc struct {
		Latch struct {
			Words []string `json:"words"`
			Match string   `json:"match"`
			Reply string   `json:"reply"`
		} `json:"latch"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("persona latch %s: %w", path, err)
	}
	// 只认 whole_trimmed：目前唯一实现的判定口径。留着字段是为了格式自解释，
	// 也是为了避免将来加别种口径时旧配置被静默按新语义解释。
	if m := strings.ToLower(strings.TrimSpace(doc.Latch.Match)); m != "" && m != "whole_trimmed" {
		return nil, fmt.Errorf("persona latch %s: match=%q 不支持（仅 whole_trimmed）", path, doc.Latch.Match)
	}
	if strings.TrimSpace(doc.Latch.Reply) == "" {
		return nil, fmt.Errorf("persona latch %s: latch.reply 为空（若不需要门锁请删除该 json）", path)
	}
	words := make([]string, 0, len(doc.Latch.Words))
	for _, w := range doc.Latch.Words {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("persona latch %s: latch.words 为空", path)
	}
	return &Latch{Words: words, Reply: doc.Latch.Reply}, nil
}
