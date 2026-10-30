# 需求 SPEC：Ollama 额度插件

状态：待确认 · 2026-10-30
作者：Ayachi（整理浩文 2026-10-30 的要求）

## 1. 要解决什么

omc 现在能显示 Ollama Cloud 的额度和余额，但那套东西在 omc 自己的控制台里，要单独部署一个
容器、一个数据库、一套密钥。CPA 本身已经有一套额度页，其他供应商都在那里看。

要的是：**Ollama 的额度也出现在 CPA 的额度页里，而且装起来跟商店里其他插件一样，装上就能用，
以后升级不用重启 CPA，不打断正在跑的推理请求。**

## 2. 给谁用

浩文自己。日常场景是：打开 CPA 管理界面，在凭证额度区看到 Ollama 还剩多少，不用再切到 omc。

## 3. 做成什么样

插件实现 CPA 的 `QuotaProvider` 接口，注册成名为 `ollama` 的额度来源。

### 3.1 额度页里能看到什么

每一条 Ollama 凭证一行，显示：

| 字段 | 说明 | 数据来源 |
|---|---|---|
| 套餐名 | 例如 Free / Pro | balance 接口或设置页 |
| 余额 | 美元金额，例如 `$12.40` | balance 接口 |
| 额度窗口 | 时段用量，例如「5 小时已用 30%」 | 设置页解析 |
| 状态 | 正常 / 警告 / 耗尽 / 取不到 | 按采集结果判定 |
| 最后采集时间 | 什么时候取到的 | 本地记录 |

取不到数据时，状态显示「取不到」并给一句原因，**不显示数字**。

### 3.2 怎么采集

按顺序试：

1. `GET https://ollama.com/api/balance`，带 `Authorization: Bearer <API Key>`
   （2026-10-30 核实：官方 OpenAPI 定义在 `get:` 下，不是 POST）
2. 失败或没有 API Key 时，`GET https://ollama.com/settings`，带 session cookie，解析 HTML

两条都失败就上报失败状态，保留上一次成功读到的值并标成「已过期」，不假装是新数据。

### 3.3 多久采一次

默认 15 分钟一次，可以在插件配置里改。支持手动刷新（CPA 的额度页有刷新按钮，走
`POST /v0/management/plugins/<id>/quota`）。

### 3.4 凭证从哪来

用 CPA 已有的凭证体系。Ollama 的 API Key 或 cookie 存在 CPA 的凭证里，插件通过
`QuotaFetchRequest.StorageJSON` / `AuthID` 读取，不自己另存一份。

## 4. 硬约束

### 4.1 升级插件不重启 CPA

- 插件必须实现 `plugin.quiesce`：收到后拒绝新请求、等手上的采集中止、释放连接池。
- 卸载后重新加载新版本，`plugin.register` 重新注册。
- **验收方法**：装 v1 → 发起一个耗时 30 秒以上的推理请求 → 请求进行中把插件换成 v2 →
  该请求必须正常返回，CPA 进程 PID 不变。

### 4.2 凭据不外泄

- cookie 和 API Key 不进日志、不进错误字符串、不进返回值、不进任何落盘文件。
- 错误信息只说"认证失败"，不带原值。
- 插件不做任何把凭证发往第三方的事，只发往固定的 ollama.com。

### 4.3 失败不返回错数

- 解析规则没匹配上 → 状态 `error`，不返回 0 或旧值冒充新值。
- 上游 429 → 按 `Retry-After` 退避，期间返回上一次的值并标「已过期」。
- 有专门的测试：给一段改版后的 HTML，断言解析器返回失败而不是错数。

### 4.4 不在 CPA 进程里闯祸

- 插件跑在 CPA 进程内，任何 goroutine panic 都可能带崩网关。
- 所有采集 goroutine 必须有 recover，异常记日志不崩溃。
- 采集有超时（默认 20 秒），不能无限挂住。

## 5. 范围

**做**：余额、额度窗口、套餐名、状态、手动刷新、定时采集、失败降级。

**不做**：
- Ollama 的 OAuth 登录流程
- 用量历史与图表
- 计费预估
- ollama 以外的供应商
- 改 CPA 本身的代码（只用公开 SDK）

## 6. 验收标准

| # | 标准 | 怎么验 |
|---|---|---|
| AC-1 | 装进 CPA 后，额度页出现 Ollama 行，显示余额和套餐 | 装插件，用 `POST /v0/management/quota/fetch`（JSON body 带 `auth_index`）看返回里有 ollama |
| AC-2 | 用 API Key 能取到余额 | 配一条真凭证，看余额数字与 ollama.com 网站一致 |
| AC-3 | 没有 API Key 时回退到 cookie 解析 | 只配 cookie，看能取到额度窗口 |
| AC-4 | 上游返回改版 HTML 时不返回错数 | 单测：喂改版 HTML，断言 status=error |
| AC-5 | 升级插件不重启 CPA，在途请求不断 | 实测：长请求进行中换插件版本，请求正常返回，PID 不变 |
| AC-6 | 凭证不出现在日志里 | 跑一轮采集，grep 日志找 cookie/key 片段，应为 0 命中 |
| AC-7 | 采集超时不会挂住 | 单测：上游不响应，20 秒后返回失败 |
| AC-8 | 插件卸载后 CPA 正常运行 | 卸载插件，CPA 其他功能不受影响 |

## 7. 许可

omc 的 ollama 解析思路源自已下线的 CPA-buddy（LGPL-3.0-only），后者又转引了
jacklee-code/ollama-cloud-quota-monitor 和 Wei-Shaw/sub2api。omc 只复用了思路、没复用源码文本，
并在源码里写明了这条来路。

本插件**同样只复用思路，从 omc 的 dev 分支重新实现**，不复制 upstream 源码。若发现必须复制
任何 LGPL 代码，停止并先确认许可证义务。

## 8. 待定

- 插件 ID 用什么（建议 `ollama-quota`）
- 是否需要支持多个 Ollama 账号（omc 支持；建议第一版支持，成本不高）
- 是否要上架官方插件商店（需要提 PR 到 `router-for-me/CLIProxyAPI-Plugins-Store`）
