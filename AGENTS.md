# Agent 指引

本仓库由 orca-issue 编排开发。编排配置见 [docs/agents/orchestration.md](docs/agents/orchestration.md)，
需求见 [docs/spec.md](docs/spec.md)。开工前两份都要读。

## 隐私与凭据

- 本仓库不存任何 Ollama 凭证、session cookie、API Key。
- 测试用 fixture，不用真实凭证。
- 日志、错误信息、返回值里不得出现凭证原文。写这类代码前先看 SPEC 的 4.2。

## 改代码时

- 只用 CPA 的公开 SDK（`sdk/pluginapi`、`sdk/pluginhost`），不碰 `internal/`。
- 新增依赖前先问；这个插件应该依赖很少。
- 采集相关的 goroutine 必须有 recover 和超时，见 SPEC 的 4.4。

## 提交

- 分支从 `dev` 切。
- 提交信息说清改了什么，引用对应 Issue（`refs #<n>`）。
- 不直接推 `main`。
