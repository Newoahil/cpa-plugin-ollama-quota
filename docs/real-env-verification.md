# 真实上游验证记录（T12-S10，amd64）

来源：`works 2026-11-12#T12-S10`，派自主会话 `776ec2b3 · 11-12 · CPA 三家凭证额度插件`。
判据：`Homan-context/inbox/agents/交付/2026-11-12-cpa-本地验证与部署路径.md` 第三节 V1～V6。

这是**开发自测记录**，不是验收结论。最终判定由跟进和浩文做。

> 红线执行情况：全程只在本机 Docker（`127.0.0.1:18700`）里跑；没有连生产 8317/8318，
> 没有发任何写请求给线上（对上游只有余额/用量 GET）；容器日志泄漏检查 0 命中。
>
> 本仓库是公开仓库，因此本文只写「一致 / 不一致」的结论与上游字段名，
> **不记录任何真实账号的余额数值、重置时刻或 key 值**。核对用的具体数字留在
> 本地验证环境的 `.runtime/`（gitignored）与本机面板上。

## 结论

| 判据 | 结果 | 一句话 |
|---|---|---|
| V1 环境自证 | **过** | 插件 `registered=true`、`effective_enabled=true`，三个 provider 都在 |
| V2 三张卡各出数 | **部分** | CMC 三个账号全字段；Ollama 只有余额（上游 balance 不返回套餐名）；**OPC 无 key，未验** |
| V3 数字和官网对得上 | **未验（人眼部分）** | 机器逐字段比对上游原始接口全部一致；官网控制台人眼比对待浩文 |
| V4 降级 | **部分** | 鉴权失败、窗口超限、网络不通：过；429、未知 status：未验 |
| V5 卸载与恢复 | **部分** | 「删了能恢复」过（经重启）；PATCH 热重扫在本环境不可用（config 只读挂载） |
| V6 凭证不落日志 | **过** | 真 key 场景 0 命中 |

**部署判据「任何一条不过就不部署」：目前不能放行。** 主要缺口是 OPC 一张真 key
和 V3 的官网人眼核对；两者都不是代码问题。

## 环境

| 项 | 值 |
|---|---|
| 日期 | 2026-11-12 17:37～18:11（UTC+8） |
| 宿主 | Windows 11 Pro 10.0.26200，Docker 29.5.3（linux/amd64） |
| 代码基线 | `2f13006`（= `origin/dev` 树，版本 0.3.0）+ 脚本提交 `46a05da` |
| 插件产物 | `dist/local/ollama-quota-v0.3.0.so`，10,795,696 字节，ELF64 x86-64（`e_machine=62`），sha256 `afad83cb87880ec96e5f9953cd8baf9f46a2f3669a6c8d176516e1a479633475` |
| CPA | `eceasy/cli-proxy-api:v8.0.47`（Version v8.0.47, Commit 90c447d），容器 `cpa-real-env-t7`，只绑 `127.0.0.1:18700` |
| 凭证 | 仓库根 `.env.real-keys`：CMC 三个账号 + Ollama API Key；OPC 与 Ollama cookie 为空 |

复现命令（Git Bash，仓库根）：

```sh
# 1) 构建 amd64 调试产物（本机无 Go，走容器）
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W)/:/src" -w /src \
  -v cpa-go-mod-cache:/go/pkg/mod -v cpa-go-build-cache:/root/.cache/go-build \
  quota-go:1.26-bookworm sh -c \
  'CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
   -buildmode=c-shared -o dist/local/ollama-quota-v0.3.0.so ./cmd/plugin'

# 2) 起环境（在 git worktree 里跑时，key 文件在主仓库根，用 CPARE_KEYS_FILE 指过去）
ASSISTANT_NO_SEND=1 CPARE_KEYS_FILE="<主仓库根>/.env.real-keys" \
  CPARE_PLUGIN="$PWD/dist/local/ollama-quota-v0.3.0.so" \
  bash scripts/real-env/cpa-real-env.sh up

# 3) 拉一轮额度 + 泄漏检查
ASSISTANT_NO_SEND=1 CPARE_KEYS_FILE="<主仓库根>/.env.real-keys" \
  bash scripts/real-env/cpa-real-env.sh verify

# 4) 面板 http://127.0.0.1:18700/management.html（管理密钥见 up 输出 / .runtime/mgmt-key）
# 5) 收工
bash scripts/real-env/cpa-real-env.sh down    # 停容器 + 删 .runtime（含生成的凭证文件）
```

## V1 环境自证 —— 过

`up` 无报错返回，末段输出：

```
plugin ollama-quota registered and effective
quota providers: ollama + command code + opencode
```

`GET /v0/management/plugins`（节选）：

```json
{"id":"ollama-quota","path":"/data/plugins/ollama-quota-v0.3.0.so",
 "configured":true,"registered":true,"enabled":true,"effective_enabled":true,
 "supports_quota":true,"quota_provider":"ollama",
 "metadata":{"version":"0.3.0"}}
```

`GET /v0/management/quota/providers`：

```json
{"providers":[{"plugin_id":"ollama-quota","provider":"ollama",
 "supported_providers":["ollama","openai-compatible-command code","openai-compatible-opencode"]}]}
```

## V2 三张卡各出数（真 key）—— 部分

`verify` 对 4 条凭证逐条 `POST /v0/management/quota/fetch`，**全部成功且带数字**：

| 凭证 | 返回（结构，数值不记录） |
|---|---|
| `cmc-1` | `plan=GOAT`；credits 余额（有具体数字）；5h 剩余 100%；7d 剩余 0% + 重置时间 |
| `cmc-2` | 同上结构，数值各不相同 |
| `cmc-3` | 同上结构，数值各不相同 |
| `ollama`（API Key 路径） | 只有 `balance`（USD，有具体数字），**无套餐名、无窗口** |

面板 `#/quota` 点「刷新全部凭证」后看到 4 张卡（3 CMC + 1 Ollama）。截图在本地
`scripts/real-env/.runtime/evidence/quota-page.png`（不进 git）。

逐字段核对 V2 的要求：

- **Command Code**：套餐名 ✓、credits 余额 ✓、5h/7d 窗口 ✓。
  **5h 窗口没有重置时间**——上游 `windowLimits.fiveHour.resetAt = 0`（上游没到重置点
  就不报）；插件按「上游未声明就不编」的原则不显示，卡片为 `-`。7d 重置时间与上游
  毫秒值换算后完全一致。
- **Ollama**：USD 余额 ✓；**套餐名缺**——上游 `/api/balance` 响应里没有 plan 字段
  （只有 `included.balance_usd` / `allowance_usd` / `purchased.balance_usd`），插件留空
  而不是猜。要出套餐名和 5h/周窗口需走 cookie 路径（设置页解析），本轮未填 cookie，未验。
- **OpenCode Go**：`.env.real-keys` 里 `OPC_KEY_1/2` 为空，**完全没有真凭证，未验**。
  解析与降级逻辑只有单测与桩覆盖，本轮不出结论。

## V3 数字和官网对得上 —— 机验一致，官网人眼比对未做

用与官网同一数据源的**上游原始接口**逐字段比对（比看网页更严），全部一致：

| 账号 | 上游原始接口字段 | 与插件输出 |
|---|---|---|
| cmc-1 | `credits.monthlyCredits`（与窗口 used 合计恰为套餐额度）；`weekly.used/cap/exceeded`；`weekly.resetAt`；`fiveHour.used/cap/resetAt`；订阅 `planId=individual-goat` | 余额取到分、7d 剩余 0%（已超限）、7d 重置时刻一致、5h 剩余 100% 且无重置、套餐 `GOAT` —— **一致** |
| cmc-2 | 同上字段 | 余额、重置时刻一致 |
| cmc-3 | 同上字段 | 余额、重置时刻一致 |
| ollama | `included.balance_usd` + `purchased.balance_usd` | 两者相加后与插件 USD 余额一致 |

口径说明：插件报的是**剩余**（`remainingFraction`），上游 CMC 的 weekly 已超限
（used > cap，`exceeded=true`），所以显示剩余 0%，不是解析失败。

**仍未做**：打开三家官网控制台人眼比对同一个账号。这需要浩文登录，脚本代替不了。
验证容器已于 2026-11-12 18:22 关闭（含真 key 生成的 `.runtime/` 一并删除）；
需要比对时按上面的复现命令重新 `up`。

## V4 降级 —— 3 过、2 未验

| 场景 | 怎么造 | 结果 |
|---|---|---|
| 鉴权失败 | 手放 `cmc-bad.json`（假 key，指向真上游） | **过**：`POST quota/fetch` 返回 502 `the Command Code API key was rejected by the credits endpoint (HTTP 401); re-enter a valid key`；面板该卡只显示「额度加载失败」+ 原因，**没有任何数字**；其余卡同时刻仍正常返回 |
| 窗口超限 | 真实 CMC 三账号的 weekly 本来就超限（`exceeded=true`） | **过**：该窗口显示剩余 0% + 重置时间，不是解析失败 |
| 网络不通 | 手放 `cmc-down.json`（`base_url=https://127.0.0.1:1`），另测 `https://10.255.255.1` | **过**：前者 0.1s、后者 5.1s 返回 502 `the request to the Command Code credits endpoint failed`；采集循环不退、容器 `RestartCount=0`，其余卡不受影响 |
| 上游 429 | 本地造不了 | **未验**（只有单测覆盖，如实记） |
| 未知 status | 桩环境内置，本轮没起桩 | **未验** |

失败卡与正常卡同屏的证据在本地 `scripts/real-env/.runtime/evidence/quota-page-degraded.png`。

## V5 卸载与恢复 —— 部分

真实上游环境把 `config.yaml` 只读挂载，管理端保存配置会直接 500
（`failed to save config: open /CLIProxyAPI/config.yaml: read-only file system`），
所以判据里的「PATCH 重扫」路径**在本环境不可用**，用重启代替：

1. 移走 `.so`（不重启）：`GET /v0/management/plugins` 里该插件 `path=""`、
   `registered=true` 仍然在——已加载的插件不会因文件消失而自动卸载（符合宿主行为：
   插件发现只在 apply 时发生）。
2. 移走 `.so` + 重启 CPA：`registered=false`、`effective_enabled=false`、`path=""`，
   `quota/providers` 为空；CPA 正常起（客户数 4，无报错）。
3. 放回 `.so` + 重启：`registered=true`、`quota providers` 恢复，日志
   `plugin loaded / plugin registered version=0.3.0`。

结论：「删了能恢复」成立；**不重启的热卸载/热重扫未验**。部署时若要热卸载，
需要能写 config 的挂载方式，否则得重启（这一条请跟进记到部署方案里）。

## V6 凭证不落日志 —— 过

`verify` 内置检查：对 `.env.real-keys` 里每个非空值 grep 容器日志，
`0 hits: none of the filled keys appear in the container log`（真 key 场景，17:37 与 17:55 两轮）。

补充自查：`.runtime/` 内除 `auths/*.json`（环境本身按设计生成的凭证文件，gitignored）
外，其余文件（config、日志副本、截图）均不含 key 值。

## 其他观察（不属于 V1～V6，供跟进判断）

1. **启动后首轮自动采集会失败一次**：两次启动（17:37:26、17:52:38）都有
   `the quota collector could not list credentials: host.auth.list did not finish
   before the deadline: context canceled`。之后 900s 的那轮（18:07）没有告警，
   手动刷新一直正常。疑似「注册时首轮 sweep 立即启动，与宿主 auth 列表就绪存在竞态」，
   影响是重启后直到下一次定时采集（默认 15 分钟）前，插件缓存可能为空。
   本轮无法判定是插件还是宿主侧时序，建议另立票。
2. **Ollama API Key 路径卡片信息少**：只有余额、没有套餐名和窗口，是上游
   `/api/balance` 不提供；浩文若要看套餐/窗口，需要填 `OLLAMA_SESSION_COOKIE`。
3. **脚本挂载限制**：`real-env` 的 `config.yaml` 是 `:ro`，管理端任何保存配置的操作
   （PATCH enabled / config）都会 500。验证够用，但热重扫类验证只能换环境做。
4. **arm64 加载仍未验**：Mac mini SSH 仍不通，按派发要求记待办，不卡本轮。
   amd64 上验的是同一份代码的行为，arm64 只做过静态校验（v0.3.0 CI 产物）。

## 未验清单（汇总）

- OPC 真 key 卡片（`OPC_KEY_1/2` 为空）
- Ollama cookie 路径（套餐名、5h/周窗口）
- V3 官网控制台人眼比对（浩文）
- 上游 429、未知 status 降级（只有单测/桩）
- V5 不重启的 PATCH 热重扫
- arm64 实机加载
