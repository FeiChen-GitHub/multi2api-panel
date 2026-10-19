// persona.go 人格提示词（persona）加载：内置人格 + 文件覆盖。
//
// 与 defaultprompt.md 的关系：defaultprompt 是「工程行为规范」，
// persona 是「人格覆写层」，两者职责不同、可同时生效：
//   - prompt.mode=custom + persona 开启：出站 system = persona 文本（人格覆写）；
//   - prompt.mode=append + persona 开启：在客户端 system 块后插 persona；
//   - persona 关闭：行为与本功能引入前完全一致（零影响）。
//
// 加载策略：**磁盘优先，embed 兜底**。
//
//   - 内置名（当前仅 "haiou"）→ 先读仓库内 personas/<name>.md；
//     读到就用磁盘版——改完 md 重启进程即生效，**不需要重新编译**；
//   - 磁盘读不到（典型：-trimpath 发布产物，或部署时没带 personas/ 目录）
//     → 回落到 //go:embed 的编译期快照，保证缺文件也能启动；
//   - 其它名字 → 当用户给的文件路径原样读，支持外挂自定义人格而不改仓库。
//
// 为什么保留 embed 而不是纯磁盘：纯磁盘方案下，忘记把 personas/ 带进部署
// 产物的后果是启动即失败；而人格层是可选功能，不该因为它让整个网关起不来。
// 正常开发流程里磁盘版与 embed 快照始终同源（同一份 md），只有部署裁剪时
// 才分叉，此时"用旧人格启动"优于"拒绝启动"。
//
// 解析失败的处置与 prompt.Load 一致：fail fast。静默回落到"无人格"会让
// 用户以为人格生效了，实际出站的是工程提示词——人格稳定性比可用性更重要，
// 宁可启动报错也不要静默失效（内置名回落 embed 是唯一例外，见上）。
package prompt

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvPersonasDir 人格目录的环境变量覆盖。
//
// 必配场景：Docker 镜像的二进制是 -trimpath 构建（Dockerfile: -trimpath -s -w），
// runtime.Caller 拿不到源文件路径，resolvePersonasDir 返回空串 → 只能走 embed，
// 容器里改 /app/personas/haiou.md 不生效。设了这个变量就能让磁盘优先在容器里
// 也成立（compose 里加 WB2A_PERSONAS_DIR=/app/personas 并挂载卷）。
const EnvPersonasDir = "WB2A_PERSONAS_DIR"

//go:embed personas/haiou.md
var builtinHaiou string

// builtinPersonas 内置人格表：名字 → embed 快照（兜底数据源，非首选）。
//
// 新增内置人格只需在此登记，对应的 md 文件放在 personas/ 下即可。
var builtinPersonas = map[string]string{
	"haiou": builtinHaiou,
}

// personasDir 仓库内人格目录（空 = 不可用，走 embed 兜底）。
var personasDir = resolvePersonasDir()

// resolvePersonasDir 解析仓库内人格目录。
//
// 基准是**本源文件**而非进程工作目录：go run（cwd=仓库根）、编译后的二进制
// （cwd 任意）、Docker（WORKDIR=/app）三种场景必须定位到同一处。
// runtime.Caller 在 -trimpath 下只剩 base name（发布产物正是这么构建的，
// 见 Dockerfile 的 -trimpath -s -w），拼不出有效目录 → 返回空串，
// 由调用方走 embed 兜底。发布产物本就不该期待"改 md 即生效"。
func resolvePersonasDir() string {
	// 环境变量优先：trimpath 发布产物（含 Docker 镜像）里 runtime.Caller
	// 不可用，只能靠显式指定。设了但目录不存在也照用——读不到时调用方
	// 会走 embed 兜底，且启动日志会打出实际用的来源，不会静默。
	if env := strings.TrimSpace(os.Getenv(EnvPersonasDir)); env != "" {
		return env
	}
	_, thisFile, _, ok := runtime.Caller(1)
	if !ok {
		return ""
	}
	dir := filepath.Dir(thisFile)
	// trimpath 场景 thisFile 退化成 "persona.go" 之类，Dir 得 "."，不可用。
	if dir == "." || dir == "" {
		return ""
	}
	return filepath.Join(dir, "personas")
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
//	name 命中内置表 → 磁盘优先，读不到才回落 embed 快照。
//	其它 → 当文件路径读（name 本身即路径）。
//
// 第二个返回值标记数据来源（"disk" / "embed" / "path" / ""），供日志与测试
// 断言"到底用了哪一份"——磁盘版与 embed 快照分叉时这是唯一的排查线索。
func LoadPersona(name string) (string, string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", "", nil
	}
	if _, ok := builtinPersonas[trimmed]; ok {
		text, err := loadBuiltinFromDisk(trimmed)
		if err != nil {
			return "", "", err
		}
		if text != "" {
			return text, "disk", nil
		}
		return builtinPersonas[trimmed], "embed", nil
	}
	raw, err := os.ReadFile(resolvePersonaPath(trimmed))
	if err != nil {
		return "", "", fmt.Errorf("persona %s: %w", trimmed, err)
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return "", "", fmt.Errorf("persona %s: file is empty", trimmed)
	}
	return text, "path", nil
}

// resolvePersonaPath 解析人格文件路径：相对路径先按进程工作目录找，
// 找不到再按**可执行文件所在目录**找。
//
// 为什么保留 cwd 优先：老部署（从 build 目录起服务）用的是 cwd 相对路径，
// 直接改成 exe 基准会让已经跑通的配置忽然读不到文件。两边都认，谁存在用谁；
// 都不存在时原样返回相对路径，让调用方报出原始配置而不是伪造一个路径。
func resolvePersonaPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return p
	}
	alt := filepath.Join(filepath.Dir(exe), p)
	if _, statErr := os.Stat(alt); statErr == nil {
		return alt
	}
	return p
}

// loadBuiltinFromDisk 读内置人格的磁盘版。
//
//   - text != ""           ：读到非空内容，用磁盘版
//   - text=="" && err==nil ：文件不存在，交还调用方走 embed 兜底
//   - err != nil           ：文件存在但读不了/内容为空 —— 明确报错，不静默兜底
//
// 为什么"文件存在但为空"要报错而不是兜底：用户改了 personas/haiou.md 想调人格，
// 手滑清空了内容，此时静默用 embed 会让"我的修改没生效"变得无法解释。
// 空文件是明确的配置错误，应该炸出来。
func loadBuiltinFromDisk(name string) (string, error) {
	diskPath := filepath.Join(personasDir, name+".md")
	raw, err := os.ReadFile(diskPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("persona %s: %s 读取失败: %w", name, diskPath, err)
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("persona %s: %s 内容为空（若想停用请设 prompt.persona_enabled=false）",
			name, diskPath)
	}
	return text, nil
}
