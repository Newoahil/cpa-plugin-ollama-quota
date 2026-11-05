# copilot-quota 设计说明

- 日期：2026-11-05
- 派发：assistant 2026-11-05#T11 · 主会话 cdddac17 · 11-05 · Ayachi 角色启动
- 前置调研：T25《Copilot 额度读取调研》（`inbox/agents/调研/2026-11-05-copilot-额度.md`）

## 1. 结构

```
copilot-quota/
  cmd/plugin/main.go            C ABI shim（cgo，-buildmode=c-shared）
  internal/copilot/             Copilot 数据源：只有一个 GET + JSON 解析
    copilot.go                  类型、状态推导、套餐映射
    client.go                   固定 URL、超时、状态分类、recover
    parse.go                    两种 payload 形状 → Snapshot
    errors.go                   四类失败 + 429 退避
  internal/quotaplugin/         CPA 侧：RPC、生命周期、额度映射
    provider.go, collector.go, rpc.go, transport.go, hostcall.go, state.go, runtime.go
    fetch.go                    reading 缓存、stale 语义、QuotaFetchResponse 映射
    credential.go               从 StorageJSON 读 github_access_token
  scripts/build.sh, scripts/abi-smoke.c
```

`internal/quotaplugin` 的脚手架（RPC 分发、热插拔多代实例、quiesce 排空、host 回调桥、
超时/recover）与 ollama-quota 同源，是同一份代码换包名和产品名；采集与解析部分按 Copilot
重写。没有引入外部依赖。

注意：脚手架注释里的 `SPEC 4.x`、`P1-a/P1-b`、`P2-1`、`#2` 等编号沿袭 ollama-quota 的 SPEC
和评审记录，是这个模块工程约束的出处；本插件没有单独的 SPEC 文档，约束以注释描述为准。

## 2. 数据流

```
CPA quota.fetch(provider=copilot)
  → rpc.handleFetch
    → provider.fetchQuota（一次 attempt 预算）
      → resolveCredential（StorageJSON，失败再走 host.auth.get）
      → copilot.Client.Fetch
        → transport（host.http.do）
          → GET https://api.github.com/copilot_internal/user
        → parseUserPayload
      → store（成功/失败/stale）
      → quotaResponse（Subscription + Summary + Groups）
```

后台 collector 每 `interval_seconds`（默认 900）扫一遍 `host.auth.list`，只处理
`provider/type == "copilot"` 的凭证；provider 为空的凭证跳过，不再像 ollama 版那样默认接管。
手动刷新直接走 `quota.fetch`，不在 collector 里排队。

## 3. 解析规则

| 输入 | 处理 |
|---|---|
| `quota_snapshots.<k>.percent_remaining` 在 [0,100] | 出 bucket，`RemainingFraction = percent_remaining/100` |
| 同一 entry 的 `entitlement` / `remaining` 都是非负有限数 | 额外带上数量，Summary 出「剩余/已用/总额」 |
| `unlimited: true` | 出 bucket，`RemainingFraction = 1`，描述写 unlimited，不造数字 |
| `percent_remaining` 缺失/越界/非数字 | 跳过该 bucket；全部桶都被跳过 → `parse_failed` |
| `limited_user_quotas` + `monthly_quotas` | `used = (total-remaining)/total`；总量缺失 → 跳过 |
| 两种形状同键 | `quota_snapshots` 优先 |
| 不认识的键，但有合法 `percent_remaining` | 照常出 bucket，标签由键名生成 |

`copilot_plan` / `access_type_sku` 只用于显示套餐，不参与额度计算。未知 SKU 不猜，回退到
`copilot_plan`；两者都没有就不显示套餐。

## 4. 失败分类

| 情况 | 分类 | 页面表现 |
|---|---|---|
| 401/403 | `auth_failed` | 重新登录该账号 |
| 404 | `auth_failed` | 「账号没有 Copilot 访问权限」 |
| 429 | `rate_limited` + `Retry-After`（缺省 30s） | 窗口内直接返回上次值并标 stale |
| 5xx、超时、DNS、连接失败 | `upstream_unreachable` | 有旧值 → stale；无旧值 → 报错 |
| 3xx | 登录页 → `auth_failed`，其他 → 网络类 | 不跟随 |
| 响应 echo 了 token | `parse_failed` | 拒绝该读数 |
| 形状认不出 | `parse_failed` | 有旧值 → stale；无旧值 → 报错 |

## 5. 安全边界

- token 只出现在发往 `api.github.com` 的 `Authorization` 头里。错误串、日志、返回值里都没有；
  `Credential.String()`/`GoString()`/`Redacted()` 都清空 token。
- 唯一 URL 是常量；测试用的 `endpoints()` 不导出。
- 拒绝 GitHub Enterprise 凭证（`github_base_url` 非 github.com），避免把企业 token 发到公网。
- **残留风险**：宿主自己的 HTTP 策略可能在返回前跟随重定向，插件看不到最终 URL。3xx 会被判为失败，
  但若宿主已跟随，token 已经发到过重定向目标。这一点取决于宿主实现，不在插件可控范围。

## 6. 验证

- `go vet ./... && go test ./... -race -count=2`：解析（付费/免费/不限/未知键/改版失败）、
  失败分类（401/403/404/429/500/超时/重定向/echo token）、stale 语义、collector 只扫 copilot、
  凭证读取与拒绝、Describe 声明。
- `scripts/abi-smoke.c`：dlopen 共享库 → init → register → identifier → describe → reset →
  quiesce → shutdown，全部通过。
- 未做：真实 CPA 实例 + 真实 Copilot 凭证的端到端对照（需要账号，属于用户节点）。
- 未做：arm64 产物的 CPA 实测（本次只验证了 ELF 架构为 arm64）。

## 7. 待确认

1. 2026 AI Credits 口径下 `copilot_internal/user` 的真实响应形状：本实现兼容任意键名 + 
   `percent_remaining`，但如果新口径换了字段名，会按设计报 `parse_failed`。需要用真账号跑一次
   T25 报告第七节的只读 curl 确认。
2. 两个账号是个人订阅还是组织席位：影响套餐显示，不影响采集。
3. 官方 billing 降级路径：第一版不做；内部接口失效后再加。
