# cpa-plugin-ollama-quota

CLIProxyAPI 插件：把 Ollama Cloud 的额度与余额接进 CPA 原生的凭证额度页。

## 这个插件做什么

CPA 有一个 `QuotaProvider` 接口，插件实现它就能把某个供应商的额度喂给 CPA 自己的额度页
（`/v0/management/credentials/quota/*`），和 Codex、Claude、Antigravity 的额度并排展示。

本插件只做一件事：读 Ollama Cloud 账号的余额和额度窗口，按 CPA 的额度模型返回。

## 硬约束

**安装和升级都不重启 CPA 进程。** CPA 的插件热加载靠换版本文件名触发；插件必须能在换版本时
安全卸载（`plugin.quiesce` → 卸载 → 加载新版本 → `plugin.register`），在途的推理请求不受影响。

**凭据不外泄。** Ollama 的 session cookie 和 API Key 是高价值凭证，不能出现在日志、错误字符串、
返回值和任何持久化快照里。

**失败降级，不返回错数。** 上游改了页面结构或接口，解析失败时返回明确的"取不到"，不能返回一个
看起来合理的错误数字。

## 数据来源

Ollama 官方没有公开额度 API。已知两条路：

1. `https://ollama.com/api/balance`（JSON，需要 API Key，Bearer 认证）——首选
2. `https://ollama.com/settings`（HTML，需要 session cookie，解析页面）——回退

omc 里已有这两条路的实现（`internal/ollama/`，`LambdaTheory/oh-my-cpa` 的 dev 分支），
解析规则是踩过坑验证过的。本插件复用那套规则，不重新发明。

## 范围

做：
- 余额（金额）和额度窗口（时段用量）
- 套餐名
- 采集失败的状态上报
- 手动刷新

不做：
- Ollama 的 OAuth 登录（走 CPA 已有的凭证体系）
- 用量历史、图表、计费预估
- 除了 ollama 之外的任何供应商

## 文档

- [需求 SPEC](docs/spec.md)
- [编排配置](docs/agents/orchestration.md)

## 许可

待定：omc 的 ollama 解析思路源自 LGPL-3.0-only 的上游项目，复用前需要核对许可证义务。
见 SPEC 的「许可」一节。
