# 热插拔实测（#4）

验证 SPEC 4.1 / AC-5：**升级插件不重启 CPA，在途推理请求不断**。

本文件记录 2026-10-30 在本机的一次真实运行。脚本是 `scripts/verify-hotswap.sh`，
随时可以重跑。

## 结论

**成立。** 一个 40 秒的流式推理请求跨过了插件从 v0.1.2 到 v0.1.3 的热替换：
请求正常结束（40/40 token、`[DONE]`、usage 块齐全），CPA 进程 PID、容器
`StartedAt`、`RestartCount` 三项全程不变，替换后 `/v0/management/plugins`
报告 v0.1.3，CPA 日志里是 `plugin hot reloaded`，没有 `error` / `fatal` / `panic`。

## 这是独立的测试实例 + fixture 环境

**这次运行没有碰浩文线上或任何已在跑的 CPA。**

| 维度 | 本次用的 | 与线上 CPA 的关系 |
|---|---|---|
| 容器 | `ollama-hotswap-test`（跑完即删） | 独立容器，独立命名 |
| CPA 端口 | `18999`（启动前探测，被占就拒绝运行） | 不共端口 |
| 插件目录 | 临时目录 `<tmp>/plugins`（`mktemp -d` 新建） | 不共目录 |
| 配置 / 凭证目录 | 同一临时目录下的 `config.yaml`、`auths/` | 不共配置、不共数据 |
| 上游 | 本机假的 OpenAI 兼容服务器（`host.docker.internal:18080`） | **不碰真实 Ollama** |
| 凭证 | fixture：`hotswap-local-mgmt-key` / `hotswap-local-client-key` | 无真实 Ollama 凭证 |

**真实 Ollama 凭证的验收是 #5，由浩文本人做。** 本文件里的任何结论都不表示
真实 Ollama 账号能被正确读取。

脚本的前置检查会在以下任一情况**直接拒绝运行**（已实测）：

- 端口 `18999` 或 `18080` 已被监听
- 容器名 `ollama-hotswap-test` / `ollama-hotswap-upstream` 已存在
- 临时目录不是全新建的

拒绝时退出码为 1，并且不启动任何东西。

## 环境

| 项 | 值 |
|---|---|
| 日期 | 2026-10-30 |
| 宿主 | Windows 11 Pro 10.0.26200，Git Bash（bash 5.x），Docker 29.5.3 |
| CPA 镜像 | `eceasy/cli-proxy-api:v8.0.47`（`Version v8.0.47, Commit 90c447d`） |
| 构建镜像 | `golang:1.26-bookworm` |
| 插件版本 | v1 = 树里提交的 `plugin.go` 的 `Version`（本次 0.1.2）；v2 = 同一份源码把 patch 号加一后的**真实第二次编译**（0.1.3） |
| Go 模块依赖 | `github.com/router-for-me/CLIProxyAPI/v8 v8.0.47` |
| 流时长 | 40 秒（每秒 1 个 token），替换发生在请求开始后约 +10 秒 |

两个版本都是 `CGO_ENABLED=1 go build -buildmode=c-shared` 产出的真实 `.so`
（约 6.2 MB，两次编译产物大小相同但内容不同），脚本用 `cmp` 断言它们不是同一份文件。构建时临时改写
`plugin.go` 的 `Version` 常量，无论成功失败都会还原；日志里的
`the tree's plugin.go is back to 0.1.2` 就是这条保证在生效。

## 实测：脚本实际输出

下面是 `bash scripts/verify-hotswap.sh` 的完整 stdout（一次通过、退出码 0）。

```text

=== preflight
    port 18999 is free
    port 18080 is free
    container name ollama-hotswap-test is unused
    container name ollama-hotswap-upstream is unused
    plugin versions: v0.1.2 (committed) -> v0.1.3 (second build)
    temp dir /tmp/ollama-hotswap.O9RDkG is new
    images present: eceasy/cli-proxy-api:v8.0.47, golang:1.26-bookworm

=== building the fixtures (fake upstream + plugin v0.1.2 and v0.1.3)
go: downloading github.com/router-for-me/CLIProxyAPI/v8 v8.0.47
go: downloading github.com/router-for-me/CLIProxyAPI/v8 v8.0.47
    v1 6166992 bytes
    v2 6166992 bytes, differs from v1
    the tree's plugin.go is back to 0.1.2

=== starting the fake upstream on 127.0.0.1:18080
    upstream healthy

=== starting the isolated CPA instance on 127.0.0.1:18999
    CPA answered

=== enabling plugin v0.1.2
    plugin registered as v0.1.2
    model fake-slow-model is routable

=== starting a 40s streaming request
    request is streaming (1 tokens so far)
    PID during the request: 3249121 (before: 3249121)

=== swapping the plugin file to v0.1.3 while the request is open
    host reports v0.1.3
    the request is still streaming across the swap

=== assertions

request timeline (UTC)
  started         2026-10-30T10:23:56Z
  swap confirmed  +10s
  ended           2026-10-30T10:24:36Z (after 40s)
  response        8207 bytes, 40/40 distinct tokens

CPA process
  PID           before=3249121  mid=3249121  after=3249121
  StartedAt     before=2026-10-30T10:23:54.819588535Z  after=2026-10-30T10:23:54.819588535Z
  RestartCount  before=0  after=0

results
  PASS  the in-flight request returned cleanly — curl exit 0
  PASS  the response carried a body — 8207 bytes
  PASS  the stream reached its terminator — saw [DONE]
  PASS  the upstream finished normally — finish_reason=stop
  PASS  the final usage chunk arrived — completion_tokens present
  PASS  every token arrived — 40/40
  PASS  the request spanned the swap — started 2026-10-30T10:23:56Z, swap at +10s, ended 2026-10-30T10:24:36Z
  PASS  the CPA process kept its PID — 3249121 throughout
  PASS  the container was not restarted — StartedAt unchanged
  PASS  no restart was counted — RestartCount 0
  PASS  the new plugin version is live — v0.1.3
  PASS  CPA logged a hot reload, not a reload from scratch — active_version=0.1.3
  PASS  CPA logged no errors — 0 error/fatal lines
  PASS  CPA did not panic — clean

PASS  hot swap verified: the request crossed the swap intact, the CPA process kept PID 3249121, and v0.1.3 is live

=== cleanup
    removed container ollama-hotswap-test
    removed container ollama-hotswap-upstream
    removed /tmp/ollama-hotswap.O9RDkG
```

## 断言逐条说明

| 断言 | 为什么这么比 |
|---|---|
| 请求正常返回（curl 0） | 替换没把连接断掉 |
| 响应体 8207 字节 | 不是空响应、不是错误页 |
| 看到 `[DONE]` 与 `finish_reason":"stop"` | 是完整结束的流，不是被截断后悄悄收尾 |
| 最后一个 usage 块在 | 末尾的分块也收到了，不是"前面几个 token 到了就算" |
| 40/40 个不同 token | 一个都没丢，也不是重复 |
| 请求**跨过**替换时间点 | 开始早于替换、结束晚于替换；否则"在途"无从谈起 |
| PID 前 / 中 / 后相同 | **只**比 PID 不够：容器被重建后可能恰好拿到同一个 PID |
| `StartedAt` 不变 | 容器没被重建，进程确实是同一个 |
| `RestartCount` 仍为 0 | 没有隐式重启 |
| 管理接口报告 v0.1.3 | 新版本真的生效了，不是只拷了文件 |
| 日志是 `plugin hot reloaded` | 走的是热替换路径，不是卸载后冷加载 |
| 日志无 `error` / `fatal` | 替换过程本身没有报错 |
| 日志无 `panic` | 插件没在 CPA 进程里出岔子 |

## 异常与告警

- **本次运行 CPA 日志里没有 `error` / `fatal` / `panic`。**
- CPA 日志里有一条与管理面板相关的 `warn`：容器在拉取管理面板资源时命中
  GitHub API 限流（403），随后从备用地址下载。这是**镜像自带的启动行为**，
  与插件、与热替换无关，也不影响额度接口和推理链路。需要的话可以在 CPA 配置里
  用 `management.disable-auto-update-panel: true` 关掉；本次没改，因为不在本票范围。
- 假上游日志里没有断连记录：请求从头到尾由 CPA 转发，替换期间没有重连。

## 热替换是怎么被触发的（给后来看脚本的人）

CPA（v8.0.47）在**应用配置**时扫描插件目录：`pluginHost.ApplyConfig` 里比较
「已加载的插件路径」和「目录里最新的插件文件」，不同就 quiesce 旧实例、加载新文件、
`plugin.register`，然后打一条 `plugin hot reloaded`。所以：

1. 新 `.so` 的文件名必须带更高的 `-v<版本>`；同名文件不会被当成新版本。
2. 光把文件放进目录不够，**要有一次配置应用**。脚本用的是
   `PATCH /v0/management/plugins/ollama-quota/config`——一次真实的插件配置写入，
   和插件页「保存」按钮走的是同一条路径。
3. 容器内 bind mount 上的文件变化**不会**通过 inotify 传到容器里的 CPA，
   所以在宿主侧 `touch config.yaml` 不触发热替换（实测如此）。脚本因此走管理接口，
   不依赖文件系统事件。

## 回滚路径（失败替换）

`plugin.register` 在 quiesce 之后仍然可用，这是热替换失败时回滚到旧实例的路径。
实测方式：先只放 v1 并注册成功，再往目录里放一个**损坏的** v0.1.3 并触发配置应用。

结果：CPA 日志报告 `failed to load plugin ... file too short`，管理接口仍然报告
`registered: true`、`version: 0.1.2`——**旧实例继续服务**，没有掉到不可用。
（这条是手工探针验证的，没有写进 `verify-hotswap.sh`：失败注入路径不稳定，
而且"坏文件不会顶掉好文件"属于 CPA 自身行为，不是本插件要证明的。单测里
`TestRollbackRegisterAfterQuiesceServesAgain` 覆盖了插件侧的一半。）

## 单测覆盖（无法只靠脚本证明的部分）

`internal/quotaplugin/hotswap_test.go`：

| 测试 | 断言 |
|---|---|
| `TestQuiesceWaitsForInFlightFetch` | quiesce 返回时，手上那次采集**已经结束**，不是"发了取消就返回" |
| `TestQuiesceReturnsWhenFetchIgnoresCancellation` | 不理会取消的采集最多拖 5 秒，不会把热替换永久卡住 |
| `TestQuiesceWithoutTrackedWorkReturnsImmediately` | 没有在途采集时 quiesce 不做 5 秒无谓等待 |
| `TestFetchAfterQuiesceIsRefusedWithAClearError` | quiesce 后 `quota.fetch` / `quota.describe` 被拒，错误码 `plugin_quiesced`，消息非空 |
| `TestPluginNeverServesInferenceSurface` | 推理 / 执行器相关的 12 个方法在 active、quiescing、stopped 三态下**全部** `unknown_method` |
| `TestRollbackRegisterAfterQuiesceServesAgain` | quiesce 后重新注册能恢复实例并继续出额度 |

`TestPluginNeverServesInferenceSurface` 是「quiesce 期间推理不受影响」这条的
**单测一半**：插件根本没有推理链路的方法可服务，所以 quiesce 不可能碰它。另一半
只有真实 CPA 能证明——就是上面那个 40 秒请求跨过替换的实测，因为 CPA 的推理请求
走的是它自己的 executor，从头到尾没有进入插件动态库。

## quiesce 与 SPEC 4.1 的差距

SPEC 4.1 要求：收到 quiesce 后**拒绝新请求、等手上的采集中止、释放连接池**。
逐条核对 #1 的实现：

| 要求 | #1 的情况 | 本票处理 |
|---|---|---|
| 拒绝新请求 | 已满足：`Quiesce` 先把 phase 置 `PhaseQuiescing`，之后 `Provider()` 返回 false，`quota.fetch` / `quota.describe` / `quota.reset` 都回 `plugin_quiesced` | 无需改动，加了测试钉住 |
| **等手上采集中止** | **缺口**：`CancelAll()` 只发取消信号就返回；`provider.Track` 的 `release` 之后才跑完，实际采集可能还在写返回值 | **已补**：`Provider` 增加 `inflight` 计数，`Quiesce` / `Shutdown` 在取消后 `drain(drainTimeout)` 等它归零，超时上限 5 秒（见下） |
| 释放连接池 | 插件自己不开连接池；采集走 CPA 注入的 transport（#3）。能释放的只有 `CancelAll` 取消掉的那些请求 | 由 drain 覆盖 |
| 推理请求不受影响 | 插件不注册任何推理 / 执行器能力 | 单测 + 实测双证（见上） |
| quiesce 后能恢复 | `Register` 在 quiesce 之后仍可用（`quiescing --register--> active`） | 无需改动，加了测试 |

### 为什么 drain 上限是 5 秒，而不是无限等

一次采集本身被 `ollama.DefaultTimeout`（20 秒）兜住，取消是协作式的，所以正常
情况下从"发取消"到"release 跑完"只差一个收尾的距离。真要无限等，一个不理会取消的
采集就会把热替换永远卡住——那比"quiesce 返回后还有个收尾没跑完"更糟。5 秒明显大于
正常收尾所需，又远小于任何宿主侧的卸载超时；超时后 quiesce 照常返回（宿主会继续
卸载），这是有意的取舍，不是遗漏。

### 没做、交回协调者的

下面几条**故意没在本票动**，因为会改 #1 的对外契约或要碰 #3 的文件：

1. **quiesce 返回值的语义。** `Quiesce()` 目前返回"调用前的 phase"，不是"是否
   drain 干净"。如果要让宿主知道 drain 超时了，得改返回值或加日志；这属于 #1 的接口，
   由协调者决定。
2. **`Register` 在 drain 未完成时是否该拒绝。** 本票试过加 `ErrDraining`，结果
   打破了 #1 明写并测试过的 `quiescing --register--> active` 迁移
   （`TestQuiesceAndRegisterCannotInterleave` 断言 quiesce 期间 register 要成功）。
   而且真实宿主里 quiesce 与 register 都在 `ApplyConfig` 的同一把 apply 锁下串行，
   这个竞态在生产里达不到，所以撤回了这个改动。是否要改这条契约，归 #1。
3. **collector 协程。** #1 的 `SetCollector` 存的是**同步**的 stop 函数；#3 的定时
   采集协程如果 stop 只是"取消不等回收"，那采集中止仍然不是屏障。本票按约定用
   `State.SetCollector` 构造了假协程验证钩子语义，但真实的采集协程要等 #3 落地后
   再按同样方式验一遍——**这是 #3 验收时要补的一条**。
4. **`host.docker.internal` 依赖。** 脚本假定假上游能从容器内访问；Docker Desktop
   默认可以。Linux 宿主需要在 run 时加 `--add-host=host.docker.internal:host-gateway`，
   脚本没加，是因为本机是 Windows + Docker Desktop，而给 Linux 加的 flag 在
   Desktop 上也没坏处——如果协调者希望脚本在 Linux CI 也能直接跑，这一行要补。

## 怎么复跑

```sh
scripts/verify-hotswap.sh            # 跑一遍，PASS 则退出码 0
scripts/verify-hotswap.sh --keep     # 保留临时目录供排查
scripts/verify-hotswap.sh --timeout 300   # 把请求上限放宽到 5 分钟
```

换端口（比如 18999 被占）：

```sh
HOTSWAP_PORT=19099 HOTSWAP_UPSTREAM_PORT=18180 scripts/verify-hotswap.sh
```

需要 `docker`、`curl`、`python3`（仅标准库）。宿主不需要 Go。
