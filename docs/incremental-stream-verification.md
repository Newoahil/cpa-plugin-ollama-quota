# T17：Ollama 推理真增量流式

来源：`works 2026-11-06#T17`，`cdddac17 · 11-06 · Ayachi 角色启动`。基线为 dev 的 `b03d9144245908fe98093d57b27f5a74a3cfc008`。这是开发自测与复跑说明，不替代独立验收。

> 状态：代码、本机标准环境与 Mac 自测完成，交独立审查。R1 的真增量/热替换项通过，整轮 exit 1 的负例串扰原件保留；经批准复用同一准确产物，在三个独立 CPA 实例完成 R2 补验，runner exit 0。R1 的失败没有改写为成功。

## 目标与实现边界

T10/T13 已证明：旧 executor 经过 `host.http.do` 完整读取上游，再同步返回 `chunks`，40-token 回答到约 40 秒才出现首字节；5 秒 idle 预算会直接失败。T13 只修正了双层 `data:`、重复 DONE 和空流挂起，没有解决全文缓冲。

本票产品提交：

| 提交 | 内容 |
|---|---|
| `63705ed` | 先红的异步流式回归，保留原有业务断言 |
| `6fa1174` | 真增量 HTTP/emit 路径、取消/终态释放及完整流生命周期屏障 |

只改插件流式路径与直接关联的计数配对问题；非流式、额度采集、凭据来源、SDK 依赖及版本号不变。不用真实账号，不改宿主，不发布。

### 两条流，两个 ID

1. 宿主交入 `stream_id` 与活跃的 `host_callback_id`；缺失时拒绝，不建立无人拥有的后台流。
2. 插件用同一 callback scope 开 HTTP operation，调用公开的 `host.http.do_stream`，请求仍固定到 `https://ollama.com/v1/chat/completions`，继续遵守宿主传输策略。
3. 上游 HTTP `stream_id` 仅供 `host.http.stream_read` / `host.http.stream_close` 使用，不能拿它当下游 executor 的 ID。
4. 同步确认 HTTP 状态和首个完整有效数据事件。空 body、注释-only、DONE-only、无效 JSON 仍在返回异步成功前报 502；HTTP 401/429 等继续走既有错误分类。
5. 返回空同步 chunk 列表，选择 SDK 的异步通道；之后每个完整事件通过 `host.stream.emit` 交给最初的 executor `stream_id`，直到 `host.stream.close`。

依赖锁定 `CLIProxyAPI/v8 v8.0.47`。SDK 的 `HTTPClient` 是 `json:"-"`，不能跨 C ABI；不能直接把 Go 接口当成原生插件 RPC 已收到的对象，也不能继续调用本身缓冲全文的 `host.http.do` 来假装增量。没有导入 CPA 的 internal 包或增加依赖。

### SSE 与内存边界

- 上游读到的是任意传输分片：可以只含半个字段、半个 UTF-8 字符，也可以包含很多帧。解析器只组装当前事件，不积攒全文。
- CRLF 和多行 data 保留语义，忽略注释及 event/id/retry 元数据；合法 JSON 压为单行，字符串内容不变。
- Payload 只有裸 JSON；不带 `data:`、分隔空行或插件自行生成的 `[DONE]`。正常完成后由宿主写唯一 DONE，不重复封帧。
- 有效数据与错误同时到达时先处理完整事件；未完成的错误分片不交出去，随后以结构化错误结束流，不把中断当成功。
- 保留原总响应 32 MiB 上限，包括元数据和注释；当前事件有界。若上游返回普通 JSON，保留单事件回退，这条回退仍须等待完整 JSON。

### 取消、异常与热替换

- HTTP operation 始终绑定原始客户端 callback scope，客户端断开会取消上游；插件侧 context 取消也由已有 cancel guard 跨 ABI 传播。
- 完成、错误和 panic 都尝试关闭上游 HTTP stream/operation 与下游 executor stream。goroutine 有 recover，错误只用现有固定分类/脱敏文案，不返回 host/upstream 原始错误或 panic 值。
- 从打开 HTTP 前到最终关闭，整个异步 pump 占一项 in-flight 计数，不只是每次回调占一下。仍不登记到 `Provider.Track`，避免热替换主动取消在途推理。
- 本票实测暴露原计数不对称：freeze 后 begin 不登记，invoke 却仍 end，可能提前消耗 pump 的计数。经批准改为 begin 返回 bool，只有实际登记的项才 end，并拒绝 freeze 后启动未登记的新流。
- 没有改变 quiesce 的超时、卸载策略或额度逻辑；宿主是否执行卸载仍受既有 SDK 行为约束，不宣称插件能修复宿主忽略 quiesce 错误的限制。

## 本机自测

全部测试显式 `ASSISTANT_NO_SEND=1`，fixture 凭据，无外部上游。

| 检查 | 结果 |
|---|---|
| 产品未改，异步/分片回归先红 | exit 1，0.228s；4096 同步 chunks、3/40-token 同步 5/42 chunks 均不符合异步契约 |
| 首轮增量实现 | exit 1，2.361s；发现真实 frozen 计数误判，以及新 fixture 错把 host panic 预期写为 502 |
| 获批的最小屏障修复、fixture 精确 host_unavailable/503 | 聚焦回归 exit 0，3.149s；取消与上游断流保持 502 分类 |
| Linux/amd64 标准容器 build / vet / 全量 race | exit 0；ollama 3.233s，quotaplugin 167.998s |
| 聚焦增量/取消/断流/背压/屏障/原空流与 3/40-token/逐字节解析，20 轮 | exit 0，2.439s |
| gofmt / diff check / 依赖和版本比较 | 通过；go.mod、go.sum、plugin.go 对基线无变化 |

标准环境命令：

```sh
ASSISTANT_NO_SEND=1 go build -mod=readonly -buildvcs=false ./...
ASSISTANT_NO_SEND=1 go vet -mod=readonly ./...
ASSISTANT_NO_SEND=1 go test -mod=readonly -race -count=1 ./...
```

原 T13 的 token、stop、usage、顺序、数量、Accept、stream、单层 SSE、唯一 DONE、空流 502 断言保留。仅将 handler 输出采集迁移为真实异步 emit，不把同步列表在测试里伪装成异步通道。新回归还覆盖：分片首事件先于上游 EOF、逐字节 UTF-8/CRLF、多行 data、4096 事件有界背压、上游数据伴随错误、客户端取消、回调 panic、超限/无效事件、frozen 回调不得消费 pump 项、drain 尝试后在途流继续完整结束。

性能/安全自查仅看变更路径：无新网络目的地、依赖或凭据来源；每事件一次 emit，自然背压，无全文事件列表；现有响应上限保留；错误和 panic 不回显秘密。真实性能对照须以下述同机 fixture 数据为准，单测时长不能当推理延迟证据。

## Mac arm64 复验

准确产品源码 `6fa11749b84bc6829d20492abcfdf66f7012c262`，基线 `b03d9144245908fe98093d57b27f5a74a3cfc008`。两份源码经 git archive 导出，两端摘要一致：

- candidate：`f042fbfe3f6ede1ae1912d1d10a2341eb4ad22df48715b7e5924a9bad6932ad3`
- baseline：`8d5a05f5808c54d9f12780094f0f7962897321af83cb3097f3a8aaa23b3d8c26`

工具来自 T10 提交 `96ecdf9` 的独立副本，原工具 hash 核对后才制取 diff。未修改旧工作区。工具改动与原始日志/产物只留本票忽略的 `dist/` 与远端隔离目录，不提交大文件或机器信息。

方法：全新目录、auth/config/state/plugins 与 internal 网络，假凭据，固定域名在隔离网络指向 HTTPS fixture，仅信任本票公钥证书，不关闭 TLS 校验。CPA 使用已有 arm64 测试镜像；记录实际 image ID 和启动版本日志，不能只凭镜像标签证明宿主准确源码。

- 同一 Mac、同一编译器/CPA 进程、同一 40-token fixture，分别编译 baseline 与 candidate。
- baseline 在途长流中 PATCH 扫描到 candidate 新目录，两个产物的版本均为 0.2.1，不改产品版本、不制造新 tag；核对在途内容和进程 identity 不变。
- 新实现跑 3-token 短流、40-token 宽松长流与 5 秒 idle 长流，保留完整 token/stop/usage/顺序/数量/单 DONE 断言。目标是首事件跟随上游，而非加大 idle 超时。
- 5 秒 idle 不变；长流另有 45 秒独立总时长预算以完成约 40 秒回答。原旧验证器的 `idle+5 秒` 总时长只够观察缓冲超时，不足以验证新实现完成 40 秒增量流；这是本票显式记录的仪器适配，不将 idle 改为 45 秒。
- 额外验证上游中断、客户端取消、空流 502、额度采集前后不变；已知 fixture canary、private-key 和 Authorization 标记扫描。
- 旧 dist 产物与现役容器只读前后比较；本票只 stop 不删。完整资源 ID、退出码、清理方式留本机交接，禁止按标签 prune。

### 准确产物与 R1 结果

Mac 重新执行 `go vet` / 全量 `go test -race`，exit 0：ollama 1.682s、quotaplugin 154.800s。Go 1.26.10 linux/arm64、GCC 12.2.0。c-shared 产物 SHA256：

- candidate（`6fa1174`）：`33eda554f22d9f4cd41e24ef114f4cff3b6b92470e2fd36a43787debf026188c`
- baseline（`b03d914`）：`e3a261ee3c420352608c1b4b0a7d5037746b1c1711dfcb3b5063da1e680cd3ac`

Mac `/usr/bin/file` 验为 ELF / ARM aarch64；Go metadata 为 c-shared、CGO=1、GOOS=linux、GOARCH=arm64，依赖 SDK v8.0.47。CPA image ID 为 `sha256:ba896394d4a291e11f76e205d3469f47996c19b1a78309a4480a0406312279cf`；实际启动自报 `dev / none / unknown`，没有据此宣称精确宿主源码版本。

同一 Mac、同一 CPA 进程、同一 HTTPS fixture 数据：

| 用例 | HTTP | 首数据 ms | 全文结束 ms | 数据字节 |
|---|---:|---:|---:|---:|
| baseline 40 token | 200 | 40147.455 | 40149.878 | 8032 |
| candidate 3 token | 200 | 3.580 | 3016.445 | 963 |
| candidate 40 token，65 秒 idle | 200 | 3.118 | 40113.729 | 8032 |
| candidate 40 token，5 秒 idle | 200 | 2.712 | 40173.745 | 8032 |

所有成功流按标准 SSE **只去一层前缀**解析，完整 token 顺序/数量、stop、usage 和唯一 DONE 通过。候选长流两个时间点（UTC）：首数据 **2026-11-06T06:56:39.654Z**，全文结束 **2026-11-06T06:57:19.765Z**。上游首帧在 `06:56:39.653774714Z` flush；首数据不再等待全文。5 秒 idle 的同一长流也完整结束，不只是收到 header。

热替换 PATCH 在 baseline 40-token 请求进行中发起；同版本新路径在旧请求完整结束后加载，宿主 PID、StartedAt、restarts 前后一致。模型发现、后台/手动额度、非流式推理及上游中途断开检查也通过。生成本身仍约 40 秒，这次改的是交付延迟，不宣称提高模型生成速度。

**R1 整轮仍是 exit 1**：18 项检查通过后，取消用例紧接断流负例，收到 `503 auth_unavailable`（10.155ms，正文引用前一条 upstream error）。CPA 日志显示前例记 502，随后该请求在路由层即 503；fixture 没收到新的推理请求。取消测试的前置状态不满足，既不算取消通过，也不是取消传播失败的实测证据。R1 原始 result、日志、SSE、driver_exit=1 和定位均保留，空流/失败后额度两项当时被阻断，未冒充已运行。11 份证据的 canary 扫描、旧 dist/现役实例/CPA identity 比较均 exit 0；宿主 loopback curl exit 7 单列 internal 网络环境项。

### R2：隔离负例补验

经批准不重跑 R1 已通过的长流；只读复用上面同一 `6fa1174` 准确 arm64 产物，复用前与复制后 SHA256 都核为 `33eda554…026188c`。未改产品、R1 原件、现役实例、cooldown 或任何已通过的断言。

R2 新目录/internal 网络/HTTPS fixture；取消、断流、空流分别使用全新 CPA 进程、auth/config/state/plugins 目录，每个实例恰好一条新假凭据。三项互不共享宿主调度状态。runner **exit 0**；三个 driver 各 exit 0，共 **22 项通过**，13 份证据 canary 扫描通过，三个实例 PID/StartedAt/restarts 与旧 dist/现役实例前后比较一致。

- **客户端取消**：HTTP 200，3.997ms 收到首个完整事件 `T01 `；客户端于 `2026-11-06T07:13:02.310Z` 取消，fixture 于 `07:13:02.311881919Z` 记录同一请求的 `stream_cancelled`，只生成 **1/40 token**，约 2ms 内传播（小于 3 秒）；无正常 DONE、无完成、无重试或 fallback。
- **上游中断**：首数据 2.935ms，约 1018.323ms 以显式错误结束；没有成功 stop/usage，没有把不完整 JSON 当事件。
- **空流**：200 上游 DONE-only 在首事件预检被拒绝，客户端得到 HTTP 502，而非挂在异步通道。
- **采集不受影响**：三个实例都在负例前后分别断言 Pro / USD 12.4 / 两窗口 5h=0.7、7d=0.2 / reset 与 fresh；每个推理负例仅一次到固定域名的已认证上游请求。

R1 保留 5 个已停止容器、1 个 internal 网络；R2 保留 8 个已停止容器、1 个 internal 网络，以及两轮 run、tools、归档、原始日志/SSE/产物。本机还保留已退出的 Linux race 容器与忽略的 dist 证据。fixture stop 后 exit 2 如实保留，不写成全部退出码为零。清理需另行授权，按进度文件和资源清单的完整 ID/挂载归属逐项核实，禁止 prune。

## 未验与交接边界

- 上游为 HTTPS fixture，不是实际 Ollama Cloud 账号；真实账号的推理与 AC-2 未验。
- 不改版本号、tag、Release 或商店；版本仍 0.2.1，dev 内容与现有 tag 不同，正式发版需另定版本。
- 不合并 PR，不删除资源、分支或会话。
- 没有界面，不需要体验环境。
