// persona.go 人格提示词（persona）加载：内置人格包 + 文件覆盖。
//
// 与 defaultprompt.md 的关系：defaultprompt 是「工程行为规范」，
// persona 是「人格覆写层」，两者职责不同、可同时生效：
//   - prompt.mode=custom + persona 开启：出站 system = persona 文本（人格覆写）；
//   - prompt.mode=append + persona 开启：在客户端 system 块后插 persona；
//   - persona 关闭：行为与本功能引入前完全一致（零影响，见 haio 分支设计）。
//
// 人格文件不进二进制（//go:embed personas/*.md 会让每次改人格都要重新编译），
// 而是在启动时按 config prompt.persona 指定的名字从磁盘读：
//   - 内置名（当前仅 "haiou"）：读仓库内 internal/prompt/personas/<name>.md，
//     路径相对于本源文件解析，因此「go run」与编译后的二进制都指向同一份文件；
//   - 其它名字：按用户给的路径原样读（支持绝对路径或相对工作目录），
//     便于在不改仓库的前提下外挂自己的人格文件。
//
// 解析失败的处置与 prompt.Load 一致：fail fast。静默回落到"无人格"会让
// 用户以为人格生效了，实际出站的是工程提示词——人格稳定性比可用性更重要，
// 宁可启动报错也不要静默失效。
package prompt

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed personas/haiou.md
var builtinHaiou string

// builtinPersonas 内置人格表：名字 → 已嵌入的文本。
//
// 新增内置人格只需在此登记，无需改调用方。
var builtinPersonas = map[string]string{
	"haiou": builtinHaiou,
}

// BuiltinPersonaNames 返回内置人格名列表（升序稳定，供面板/文档展示）。
func BuiltinPersonaNames() []string {
	names := make([]string, 0, len(builtinPersonas))
	for n := range builtinPersonas {
		names = append(names, n)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// LoadPersona 按名字加载人格文本。
//
//	name 空 → 返回空串、无错误（= 人格未配置，调用方据此关闭人格层）。
//	name 命中内置表 → 返回内置文本。
//	其它 → 当文件路径读（name 本身即路径）。
//
// 第二个返回值标记是否命中内置表——调用方（配置层）要区分「内置名」与
// 「自定义路径」以给出不同的错误提示，故不能只靠 error。
func LoadPersona(name string) (string, bool, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", false, nil
	}
	if text, ok := builtinPersonas[trimmed]; ok {
		return text, true, nil
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		return "", false, fmt.Errorf("persona %s: %w", trimmed, err)
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return "", false, fmt.Errorf("persona %s: file is empty", trimmed)
	}
	return text, false, nil
}
