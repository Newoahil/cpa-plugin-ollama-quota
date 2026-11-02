# 热插拔实测（#4）

验证 SPEC 4.1 / AC-5：**升级插件不重启 CPA，在途推理请求不断**。

本文件记录 2026-10-30 在本机的一次真实运行。脚本是 `scripts/verify-hotswap.sh`，
随时可以重跑。

## 结论

**AC-5（在途请求不断、PID 不变）成立。** 一个 40 秒的流式推理请求跨过了插件从
v0.1.2 到 v0.1.3 的热替换：请求正常结束（40/40 token、`[DONE]`、usage 块齐全），
CPA 进程 PID、容器 `StartedAt`、`RestartCount` 三项全程不变，替换后
`/v0/management/plugins` 报告 v0.1.3，CPA 日志里是 `plugin hot reloaded`，
没有 `error` / `fatal` / `panic`，也没有 quiesce drain 超时。

**但 SPEC 4.1 的「等手上的采集中止」只有部分验证，不能整体标为满足。**
合作取消路径已验证；不理会取消的采集中止不了，5 秒后放弃并把超时**记录/上报**出来，
宿主仍会卸载插件（改这一点需要改 CPA，另立票）。逐条见「quiesce 与 SPEC 4.1 的差距」。

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

脚本的前置检查会在以下任一情况**直接拒绝运行**：

- 端口 `18999` 或 `18080` 已被监听
- 容器名 `ollama-hotswap-test` / `ollama-hotswap-upstream` 已存在
- 运行中的容器把某个目录挂进了临时目录（相等 / 祖先 / 后代 / 根目录都算；Windows 路径按大小写不敏感）
- 上面这项检查本身跑不起来（`docker ps` / `docker inspect` 失败、helper 崩溃）——检查失败也算拒绝
- 临时目录不是全新建的

拒绝时退出码为 1，**不启动任何容器，也不执行任何 `docker rm`**（包括不会删掉那个
同名的、别人的容器）。这条不是靠读代码保证的：`scripts/verify-hotswap-guards.sh`
用记录型 `docker` 替身逐条验证了「拒绝路径 `docker rm` 调用数为 0」，
证据见下面的「安全前置与反向验证」。

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
    temp dir /tmp/ollama-hotswap.6rOIxY is new
    checking 3 running container(s) for mount overlap
    no running container mounts a tree containing /tmp/ollama-hotswap.6rOIxY
    images present: eceasy/cli-proxy-api:v8.0.47, golang:1.26-bookworm

=== building the fixtures (fake upstream + plugin v0.1.2 and v0.1.3)
go: downloading github.com/router-for-me/CLIProxyAPI/v8 v8.0.47
go: downloading github.com/router-for-me/CLIProxyAPI/v8 v8.0.47
    v1 6172848 bytes
    v2 6172848 bytes, differs from v1
    the working tree's plugin.go was never modified (still v0.1.2)

=== starting the fake upstream on 127.0.0.1:18080
    upstream container 78575846a08cd528a1b1f0be6645c4108026d19488cad8ba7d8117e763d6ac27
    upstream healthy

=== starting the isolated CPA instance on 127.0.0.1:18999
    CPA container 76bc8d866205d7a17fb26f0d66a096e543fa7fc75299288faff486eca3d49461
    CPA answered

=== enabling plugin v0.1.2
    plugin registered as v0.1.2
    model fake-slow-model is routable

=== starting a 40s streaming request
    request is streaming (1 tokens so far)
    PID during the request: 3448982 (before: 3448982)

=== swapping the plugin file to v0.1.3 while the request is open
    host reports v0.1.3
    the request is still streaming across the swap

=== assertions

request timeline (UTC)
  started         2026-10-30T11:13:42Z
  swap confirmed  +10s
  ended           2026-10-30T11:14:22Z
  measured        40s (the acceptance gate needs >30s)
  response        8207 bytes, 40/40 distinct tokens

CPA process
  PID           before=3448982  mid=3448982  after=3448982
  StartedAt     before=2026-10-30T11:13:41.131558951Z  after=2026-10-30T11:13:41.131558951Z
  RestartCount  before=0  after=0

results
  PASS  the in-flight request returned cleanly — curl exit 0
  PASS  the response carried a body — 8207 bytes
  PASS  the stream reached its terminator — saw [DONE]
  PASS  the upstream finished normally — finish_reason=stop
  PASS  the final usage chunk arrived — completion_tokens present
  PASS  every token arrived — 40/40
  PASS  the request spanned the swap — started 2026-10-30T11:13:42Z, swap at +10s, ended 2026-10-30T11:14:22Z
  PASS  the request outlived 30 seconds — measured 40s
  PASS  the CPA process kept its PID — 3448982 throughout
  PASS  the container was not restarted — StartedAt unchanged
  PASS  no restart was counted — RestartCount 0
  PASS  the new plugin version is live — v0.1.3
  PASS  CPA logged a hot reload, not a reload from scratch — active_version=0.1.3
  PASS  no quiesce drain timed out — every quiesce drained within the bound
  PASS  CPA logged no errors — 0 error/fatal lines
  PASS  CPA did not panic — clean

PASS  hot swap verified: a 40s request (measured) crossed the swap intact, the CPA process kept PID 3448982, and v0.1.3 is live

=== cleanup
    removed the CPA container this run started (76bc8d866205d7a17fb26f0d66a096e543fa7fc75299288faff486eca3d49461)
    removed the upstream container this run started (78575846a08cd528a1b1f0be6645c4108026d19488cad8ba7d8117e763d6ac27)
    removed build scratch /tmp/hotswap-src.uBXjSH
    removed /tmp/ollama-hotswap.6rOIxY
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
| `TestQuiesceWaitsForInFlightFetch` | **合作取消**路径：quiesce 返回时手上那次采集已经结束，且 `LastDrain` 报告 `TimedOut=false` |
| `TestQuiesceTimeoutIsObservable` | **不理会取消**的采集：5 秒后放弃，`LastDrain` 记录 `TimedOut=true, InFlight=1`；不假装成功 |
| `TestQuiesceTimeoutSurfacesAsErrorEnvelope` | 同一场景的 RPC 面：返回 `ok:false`、code `quiesce_drain_timeout`、`detail.in_flight`，phase 仍是 `quiescing` |
| `TestCleanQuiesceEnvelopeIsSuccess` | 反面对照：干净 drain 仍是普通成功，超时码只在真超时出现 |
| `TestQuiesceWithoutTrackedWorkReturnsImmediately` | 没有在途采集时 quiesce 不做 5 秒无谓等待 |
| `TestFetchAfterQuiesceIsRefusedWithAClearError` | quiesce 后 `quota.fetch` / `quota.describe` 被拒，错误码 `plugin_quiesced`，消息非空 |
| `TestPluginNeverServesInferenceSurface` | 推理 / 执行器相关的 16 个真实 ABI 方法在 active、quiescing、stopped 三态下**全部** `unknown_method` |
| `TestRollbackRegisterAfterQuiesceServesAgain` | quiesce 后重新注册能恢复实例并继续出额度 |

`TestPluginNeverServesInferenceSurface` 是「quiesce 期间推理不受影响」这条的
**单测一半**：插件根本没有推理链路的方法可服务，所以 quiesce 不可能碰它。另一半
只有真实 CPA 能证明——就是上面那个 40 秒请求跨过替换的实测，因为 CPA 的推理请求
走的是它自己的 executor，从头到尾没有进入插件动态库。

## quiesce 与 SPEC 4.1 的差距

SPEC 4.1 要求：收到 quiesce 后**拒绝新请求、等手上的采集中止、释放连接池**。
逐条核对 #1 的实现：

| 要求 | 状态 | 证据与边界 |
|---|---|---|
| 拒绝新请求 | 已验证 | `Quiesce` 先把 phase 置 `PhaseQuiescing`，之后 `Provider()` 返回 false，`quota.fetch` / `quota.describe` / `quota.reset` 都回 `plugin_quiesced`。`TestFetchAfterQuiesceIsRefusedWithAClearError` 钉住 |
| **等手上的采集中止** | **部分验证，不是无条件屏障** | 见下面「合作取消已验证、无界阻塞未证明」一节 |
| 释放连接池 | **未证明** | 当前只有占位实现，插件不持有自己的连接池，`drain` 不是关闭池。采集走 CPA 注入的 transport（#3）；那些连接的归属与释放在 #3，本票不能声称覆盖。**不要**把这条读成「已满足」 |
| 推理请求不受影响 | 已验证（正常热替换场景） | 插件的 16 个真实 ABI 推理/执行器方法在 active、quiescing、stopped 三态都回 `unknown_method`；另有 40 秒长流跨替换的实测。**不能**外推到 drain 超时场景 |
| quiesce 后能恢复 | 已验证（插件侧） | `Register` 在 quiesce 之后仍可用（`quiescing --register--> active`），`TestRollbackRegisterAfterQuiesceServesAgain` 覆盖 |

### 合作取消已验证、无界阻塞未证明（评审 R2）

精确地说，本票证明的和没证明的分开是：

- **已验证**：一个遵守取消的采集，在 `Quiesce` 返回前一定已经跑完 `release`。
  `TestQuiesceWaitsForInFlightFetch` 直接断言这一点，`TestCleanQuiesceEnvelopeIsSuccess`
  断言这种情况下 RPC 回的是普通成功。
- **未证明**：一个**不理会**取消的采集。该 tracked fetch 测试的等待预算为 5 秒，此时
  `TestQuiesceTimeoutIsObservable` / `TestQuiesceTimeoutSurfacesAsErrorEnvelope`
  断言超时被**观察到**：`State.LastDrain()` 记录 `TimedOut=true, InFlight=N`，
  RPC 返回 `ok:false` + code `quiesce_drain_timeout` + `detail.in_flight`。
  **但宿主仍会卸载插件**：CPA v8.0.47 的 `callQuiesce` 不看 quiesce 的返回，
  换版本照常继续。所以「quiesce 返回时所有采集都结束了」这句**不成立**，
  本票不这样写。

要让宿主在有在途采集时拒绝替换，必须改 CPA（本票硬边界之外），另立票。

### 当前 drain 上限（合并后更正）

当前 `State.drain` 串行判定三组工作：collector stops、`Provider.Track` 取数、
宿主回调。最坏等待上界约「各 collector stop 预算之和 + 5 秒 + 5 秒」，
单个默认 collector 是 22 + 5 + 5，约 32 秒；脱离的 collector 还会增加各自的 stop 预算。
**正常情况远小于这个值**，工作完成就继续，没有工作时不会等满预算。
超时统一返回 `quiesce_drain_timeout`，带 `detail.in_flight`、`detail.host_pending`、
`detail.unsettled`；这个有界等待不是所有回调都结束的无条件屏障。
前面 2026-10-30 的 5 秒超时案例只描述当时的 tracked fetch 测试，不能当作当前 quiesce 总上界。

### 没做、交回协调者的

下面几条**故意没在本票动**，因为会改 #1 的对外契约或要碰 #3 的文件：

1. **quiesce 的返回值语义保持 #1 原样。** `Quiesce()` 仍返回「调用前的 phase」。
   drain 结果走独立的 `State.LastDrain()` + RPC 错误 envelope，不侵占 #1 的契约；
   要让宿主据此拒绝替换，需要改 CPA，已记入上面的边界说明。
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

## 安全前置与反向验证（评审 R1 / R3 / R4 / R5 的修复）

脚本最重要的性质是**它不做什么**。那些性质只在拒绝路径和中断路径上执行，
平时跑一次成功是碰不到的，所以另有一份替身验证：
`scripts/verify-hotswap-guards.sh`（不连真实 Docker，用记录型 `docker` 替身）。

| 性质 | 怎么保证的 | 替身验证 |
|---|---|---|
| 拒绝路径不删任何容器 | 所有前置检查都在第一个 `docker run` 之前；`OWNED_CPA_ID` / `OWNED_UPSTREAM_ID` 只在真正启动后才有值，清理只按这两个 **ID** | 同名容器、端口被占、挂载重合三种拒绝，都是退出 1 且 `docker rm` 调用数 **0** |
| 清理只删自己创建的 | `docker run -d` 的输出存成容器 ID，`cleanup` 只 `docker rm -f <ID>`，从不按名字 | 直接跑抽出来的 `cleanup`：给出两个 ID 时正好删这两个；不给 ID 时 `docker rm` 调用数 0 |
| 构建不改工作区源码 | 构建在 `mktemp -d` 的**副本**里做（`stage_build_tree`），`plugin.go` 在副本里盖章 | 脚本内有构建后断言；替身验证还核对 `plugin.go` 与提交版本逐字节相同 |
| 挂载不重合 | 前置检查读所有运行中容器的 `Mounts[].Source`，与临时目录比相等/祖先/后代；根目录 `/` 是任何绝对路径的祖先，Windows 盘符路径按大小写不敏感比较 | 替身返回一个挂载父目录的容器 → 退出 1、`docker rm` 0 次、**没有启动任何容器**；另**直接调用真实 helper** 验六种路径形状（相等 / candidate 祖先 / candidate 后代 / 根 / Windows 不同大小写 / 无关） |
| 挂载检查失败必须拒绝 | 只接受 helper 退出码 1（明确「无重合」）才继续；0 = 有重合，其它 = 失败，`docker ps` / `docker inspect` 失败也拒绝 | 替身让 `inspect` 失败、`ps` 失败、helper 崩溃三种情况各自退出 1 并打印诊断，**0 次 `docker run`、0 次 `docker rm`** |
| 短流不给验收结论 | `HOTSWAP_STREAM_SECONDS < 31` 直接拒绝（除非 `--debug`）；结尾用**实测**耗时断言 > 30 秒 | 6 秒流 → 退出 1，输出里没有任何 `PASS  hot swap verified` 行 |

复跑替身验证：

```sh
scripts/verify-hotswap-guards.sh      # 36 项全 PASS 才退出 0，全程不碰真实 Docker
```

其中路径判定那一组是**直接调用真实 `scripts/mount_overlap.py`**（六种路径形状），
不是替身——替身只能证明「收到重合结果会拒绝」，证明不了「重合都发现得了」。

`mount_overlap.py` 的退出码本身就是契约：`0` = 有重合，`1` = 明确无重合，
`2` = 用法错误，`3` = 内部错误。脚本只把 `1` 当作可以继续，其余一律诊断并拒绝，
所以检查自身坏掉不会被误读成「安全」。

**说明「拒绝即无影响」的证据边界**：替身证明的是**脚本调用了什么 Docker 命令**——
拒绝路径上没有任何 `rm`，因此不可能删掉别人的容器。它不能证明真实 Docker 的其它
副作用（本机替身不执行真实容器），但脚本在拒绝路径上根本没有开始任何容器，这一点
由调用记录直接可见。

### 退出时删临时目录的例外

断言失败时脚本会 `KEEP=1` 保留临时目录（诊断需要），所以**不是**所有退出路径都删目录；
容器始终按 ID 删除。这条例外是有意的，不要读成「所有退出都清理干净」。

## 怎么复跑

```sh
scripts/verify-hotswap.sh                 # 验收跑：请求必须实测 >30s，PASS 则退出码 0
scripts/verify-hotswap.sh --keep          # 保留临时目录供排查
scripts/verify-hotswap.sh --debug         # 短流程调试：绝不打印 PASS
scripts/verify-hotswap.sh --timeout 300   # 把请求上限放宽到 5 分钟
scripts/verify-hotswap-guards.sh          # 反向验证（替身，不碰真实 Docker）
```

**「脚本 PASS」到底证明了什么**：它证明请求**实测**跨过了 >30 秒（脚本自己记的开始/结束
时间戳），替换发生在请求进行中，响应完整，PID / StartedAt / RestartCount 不变，
新版本生效，且日志里没有 quiesce drain 超时。短于 31 秒的配置会被拒绝，`--debug`
跑得再干净也只打印 `DEBUG RUN OK`，不会给验收结论。

换端口（比如 18999 被占）：

```sh
HOTSWAP_PORT=19099 HOTSWAP_UPSTREAM_PORT=18180 scripts/verify-hotswap.sh
```

需要 `docker`、`curl`、`python3`（仅标准库）。宿主不需要 Go。

## 首次安装：从未启用插件子系统到插件生效

2026-11-02 实测 `bash scripts/verify-first-install.sh` 退出 0，打印 PASS。
**经实测更正：首次安装也不需要重启 CPA。** 不推断早期验证经过。
推理上游是沿用热替换脚本的 Go SSE 替身，不是真实 Ollama，不验证真实账号额度。

| 环境 | 值 |
|---|---|
| CPA / 构建镜像 | `eceasy/cli-proxy-api:v8.0.47` / `golang:1.26-bookworm` |
| 宿主 | Windows / Git Bash / Docker Desktop |
| 隔离 | 全新独立容器；全新临时目录独立 bind mount config、plugins、auths；不复用业务数据卷 |
| 端口 | CPA `127.0.0.1:19999`，假上游 `127.0.0.1:19080` |
| 初始状态 | `plugins.enabled: false`，插件目录为空；该实例从未启用插件 |
| 构建 | 证明初始状态后，在源码临时副本的 Go 容器内运行 `bash scripts/build.sh --go`，产出 `dist/ollama-quota-v0.1.2.so` |
| 实际启用路径 | **`PATCH /v8/management/config`** → HTTP 200 → 运行中加载 |

配置写入前缀必须用 v8：**v8.0.47 没有 `PATCH /v0/management/config`**，
另一次隔离实测的 v0 PATCH、PUT 均为 HTTP 404；v0 的配置入口只有 `GET /config`
与 `GET/PUT /config.yaml`。源码的 v8 ConfigV8 在管理保存后直接触发 ApplyConfig，
不依赖文件系统 watcher；本次实测验证了该路径。README 中有完整 PATCH 示例。

时间线（UTC；原始日志为 UTC+8）：

| 时间 | 事件 |
|---|---|
| 04:24:55.947775271 | CPA StartedAt；04:24:56 脚本记录容器创建完成 |
| 启动后、长请求前 | 总开关 false、plugins 空数组、日志无 plugin loaded；随后编译、复制 .so |
| 04:25:17 | 开始 40 秒 SSE 请求，收到首 token 后等 8 秒 |
| 04:25:27 | PATCH 配置；HTTP 200；日志 plugin loaded、plugin registered；API 确认注册 |
| 04:25:57 | 长请求完整结束；随后清理本次两个容器及全部临时目录 |

| 断言 | 结果 | 证据 |
|---|---|---|
| 初始未启用、未加载 | PASS | plugins_enabled 为 false、plugins 空数组，日志无 plugin loaded |
| 管理接口写入 | PASS | `PATCH /v8/management/config -> HTTP 200`，响应 status 为 ok、config-version 为 8 |
| 加载与注册日志 | PASS | `pluginhost: plugin loaded plugin_id=ollama-quota version=0.1.2`；`pluginhost: plugin registered plugin_id=ollama-quota` |
| 管理面注册 | PASS | id 为 ollama-quota、registered 为 true、version 为 0.1.2 |
| 在途长流完整 | PASS | curl exit 0；8207 bytes；40/40 distinct tokens；`[DONE]`、`finish_reason=stop`、usage 全到 |
| 跨 PATCH 首次启用 | PASS | 实测 40s；启用确认 +10s，确认后 curl 仍在运行 |
| PID 不变、进程未重启 | PASS | before / mid / after 都是 `884421` |
| StartedAt 不变 | PASS | `2026-11-02T04:24:55.947775271Z` 前后相同，中途完整 identity 也相同 |
| RestartCount 不变 | PASS | 前 / 中 / 后均为 `0` |
| 无 error/fatal/panic | PASS | `0 error/fatal lines`，`CPA did not panic — clean` |

关键原始输出：

```text
=== assertions

request timeline (UTC)
  started         2026-11-02T04:25:17Z
  enable confirmed  +10s
  ended           2026-11-02T04:25:57Z
  measured        40s (the acceptance gate needs >30s)
  response        8207 bytes, 40/40 distinct tokens

CPA process
  PID           before=884421  mid=884421  after=884421
  StartedAt     before=2026-11-02T04:24:55.947775271Z  after=2026-11-02T04:24:55.947775271Z
  RestartCount  before=0  after=0

[2026-11-02 12:25:27] [--------] [info ] [host.go:362] pluginhost: plugin loaded plugin_id=ollama-quota version=0.1.2 path=/data/plugins/ollama-quota-v0.1.2.so
[2026-11-02 12:25:27] [--------] [info ] [host.go:385] pluginhost: plugin registered plugin_id=ollama-quota plugin_name=ollama-quota version=0.1.2 path=/data/plugins/ollama-quota-v0.1.2.so
  CPA started     2026-11-02T04:24:56Z
  enable began    2026-11-02T04:25:27Z
  registered      2026-11-02T04:25:27Z
  enable path     PATCH /v8/management/config
results
  PASS  the in-flight request returned cleanly — curl exit 0
  PASS  the response carried a body — 8207 bytes
  PASS  the stream reached its terminator — saw [DONE]
  PASS  the upstream finished normally — finish_reason=stop
  PASS  the final usage chunk arrived — completion_tokens present
  PASS  every token arrived — 40/40
  PASS  the request spanned first enable — started 2026-11-02T04:25:17Z, enable at +10s, ended 2026-11-02T04:25:57Z
  PASS  the request outlived 30 seconds — measured 40s
  PASS  the CPA process kept its PID — 884421 throughout
  PASS  the container was not restarted — StartedAt unchanged
  PASS  no restart was counted — RestartCount 0
  PASS  the new plugin version is live — v0.1.2
  PASS  CPA logged plugin loaded plugin_id=ollama-quota
  PASS  CPA logged plugin registered plugin_id=ollama-quota
  PASS  all identity fields unchanged before/during/after
  PASS  initial subsystem disabled without loaded plugin — checked API and logs before copying .so
  PASS  no quiesce drain timeout was logged — first installation does not exercise replacement drain
  PASS  CPA logged no errors — 0 error/fatal lines
  PASS  CPA did not panic — clean

PASS  first install verified: a 40s request (measured) crossed first enable intact, the CPA process kept PID 884421, and v0.1.2 is live

=== cleanup
    removed the CPA container this run started (f7af78dbb6e8fe7a9e65000455a742bc203c4f3de557863fbd493c47ba297a0f)
    removed the upstream container this run started (9284fb5b6c76f3a49cabd1c987e33e11c47303d2e3fed90db06bf06fe30fd513)
    removed build scratch /tmp/first-install-src.dZ5y0I
    removed /tmp/ollama-first-install.oONX1L
```

备选文件路径也单独实测通过：04:20:21—04:21:01 的 40 秒流跨过容器内
原位编辑配置与 watcher 加载（04:20:31），40/40 token 全到，PID 全程 878549，
StartedAt 和 RestartCount 不变。Docker Desktop 的宿主 bind mount 写入不保证 inotify，
因此该次是在隔离容器内生成临时内容再 `cat` 原位覆盖；单文件挂载不能用 `sed -i` 重命名。
优先使用上面已经跑通的 v8 管理接口。

复跑：`bash scripts/verify-first-install.sh`。端口变量是 `FIRST_INSTALL_PORT`、
`FIRST_INSTALL_UPSTREAM_PORT`；拒绝 18999 / 18080 和相同端口。沿用热替换脚本的
端口、容器名、挂载祖先/后代检查，所有 preflight 拒绝都在任何容器创建前。
清理只使用本次 `docker run -d` 返回的 CID；构建容器用 `--rm`，退出时终止本次 curl
并清理全部临时目录（失败也清理）。短流仅允许 `--debug`，不给验收 PASS。
首次安装不覆盖替换时的 drain；本次没有发现插件代码问题，没有改 `internal/` 或新增依赖。
