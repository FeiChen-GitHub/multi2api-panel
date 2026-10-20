# config.json 逐字段说明

`config.json` 不支持注释，本文即注释。缺省值指 `config.example.json` 的取值。

## prompt

| 字段 | 缺省 | 说明 |
|---|---|---|
| `prompt.mode` | `passthrough` | 系统提示词模式。`passthrough` = 透传客户端原始 system（降级重试时切中性提示词）；`custom` = 网关自有提示词**替换**客户端 system/developer；`append` = 客户端 system 保留，网关提示词**追加**在其后（既有消息逐字不动） |
| `prompt.file` | 空 | 自定义提示词文件路径。空 = 内置默认（`internal/prompt/defaultprompt.md`，约 2KB）。路径非空但不可读 → **启动报错**（fail fast，不静默回落内置默认） |
| `prompt.persona_enabled` | `false` | 人格层总开关。缺省关闭保证既有部署零影响；开启后 `prompt.persona` 必须非空，否则**启动报错** |
| `prompt.persona` | 空 | 人格名或文件路径。内置名 `haiou` 读仓库内 `internal/prompt/personas/haiou.md`；其它字符串按文件路径读（如 `internal/prompt/personas/workflow.md`），支持外挂自定义人格。文件不可读 / 内容为空 → **启动报错** |
| `prompt.persona_mode` | `append` | 人格注入方式。`append` = 保留客户端 system，人格插在其后（缺省，工具约定与项目规范不被抹掉）；`replace` = 人格**覆盖**客户端所有 system/developer。非法值 → **启动报错** |

## 人格层生效规则

- 人格开启后接管注入内容，`prompt.mode` 只管工程提示词层，两者解耦。
- 降级期（上游内容拦截后）人格让位给 Degraded 中性提示词，同一次重试生效。
- 加载策略：磁盘优先、`//go:embed` 兜底（`internal/prompt/persona.go`）。源码运行 / 非 trimpath 构建改 md 重启即生效；trimpath 发布产物（含 Docker）需设 `WB2A_PERSONAS_DIR` 或重编译。
- Docker：镜像已带 `/app/personas` 并预设 `WB2A_PERSONAS_DIR=/app/personas`，挂载 `./internal/prompt/personas:/app/personas` 可直接在宿主机编辑。
- 启动日志打印实际来源：`[config] persona "..." loaded from disk (N bytes, mode=...)`。磁盘版与 embed 快照分叉时这是唯一排查线索。

## 内置人格文件

| 文件 | 用途 |
|---|---|
| `internal/prompt/personas/haiou.md` | 海鸥人格（内置名 `haiou`，约 9KB） |
| `internal/prompt/personas/workflow.md` | 工程执行工作流（约 2KB，产物优先、缺参保守推进、收束格式） |

两者均为纯文本，随时可改，改完重启进程生效（同上加载策略）。
