# AGENTS.md

本文件记录在本仓库工作的协作约定，供 Codex / 其它 agent 读取。

## Git：拉取与推送必须以 fetch 的真实结果为准

背景（2026-10-16 事故）：会话开始执行 `git pull` 时，网络到 `github.com:443`
不通，`git pull` / `git pull --ff-only` / `git fetch --all` 全部报
`Recv failure: Connection was reset` / `Failed to connect to github.com port 443`。
但当时拿不联网的 `git status -sb` 看到 `up to date` 就误报「已拉取最新」，
本地实际仍停在旧的远端跟踪引用上，几小时后的推送被拒（远端已有 3 个新提交）。

约定：

- `git status` 的 `up to date` / `nothing to commit` 只反映**本地跟踪引用**，
  fetch 失败时会给出假象，不能作为「已同步远端」的依据。
- 每次拉取 / Push 前先 `git fetch`，依据其**退出码与输出**判断结果。
- fetch / pull 失败时，如实报告「网络不通、拉取失败」，**不得**宣称「已是最新」。
- Push 被拒（`fetch first` / `non-fast-forward`）时按 fetch → rebase/merge → 再 push 处理。
