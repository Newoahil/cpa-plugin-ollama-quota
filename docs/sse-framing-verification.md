# T13：Ollama 流式封帧修复与复验

来源：`works 2026-11-06#T13`，`cdddac17 · 11-06 · Ayachi 角色启动`。问题依据 T10 的 Mac arm64 CPA + HTTPS fixture 实测；不使用真实 Ollama 账号。这是开发自测记录，最终验收由跟进判定。

> 当前状态：最终准确代码源码 `fb79c29` 已重新完成 Mac 全量 vet/race、arm64 c-shared 构建与真实 CPA + HTTPS fixture 验证，runner exit 0，driver 15/15 项通过。空流防护纳入同轮源码；整段缓冲保留，交跟进审查，不代替最终验收。

## 修复边界与根因

基线：`093d1f8b876911f14f1c4e91d6ef7eeedceefece`。

| 代码提交 | 内容 |
|---|---|
| `07b7a097fea02a0ccf79c9f40dee2a1f61c2a548` | 修复双层 SSE 外壳与插件重复 DONE，补宿主封帧回归 |
| `fb79c292ffa70f877a88fa231067f35a9bc1160a` | 经跟进批准，防护去除 DONE 后直接引入的空流回归 |

插件声明输入/输出都是 `chat-completions`。锁定 SDK `v8.0.47` 的同格式 adapter 原样转交 Payload，OpenAI handler 再写 `data: %s\n\n`，通道关闭后自己写 DONE。旧实现把完整的 `data: {JSON}\n\n` 当成 Payload，又自己追加 `data: [DONE]\n\n`，于是客户端收到 `data: data: {JSON}` 和重复终止事件；旧单测恰好断言了这个错误的完整帧契约。

修复仅改变这个边界：

- `ExecutorStreamChunk.Payload` 只交出 SSE 事件 data，不交出 `data:` 和帧间空行；宿主负责外层 SSE 与最终唯一的 `[DONE]`。
- 跳过上游 DONE、注释和 event/id/retry 元数据。同一事件的多个 data 字段按 SSE 规则连接；合法 JSON 用 `json.Compact` 去掉格式换行，避免宿主单个 data 字段出现未加前缀的物理换行，字符串内容不变。
- 非 event-stream 的合法 JSON 回退同样返回裸 JSON。
- handler 对零 data chunk 返回现有 `upstream_invalid_response` / HTTP 502，不让同步插件误入 SDK 的异步 emit 等待分支，详见下节。
- **整段缓冲不改**：仍完整读取上游后返回同步 chunk 列表；transport、推理超时、SDK、格式声明、凭据来源、非流式路径与依赖都未改。

精确契约以锁定版本源码和实际 CPA 复验为准，不把最新公开文档当成锁定版本的实现证明。

## 回归与本机自测

全部测试命令与容器带 `ASSISTANT_NO_SEND=1`，仅使用 fixture。经跟进批准纠正两条错误期望，未删减或放宽 token、stop、usage、顺序、数量、Accept、stream 业务断言。

| 检查 | 准确源码/阶段 | 结果 |
|---|---|---|
| 新增/纠正流式断言，产品未改 | 基线产品实现 | **红灯，exit 1**；单层 SSE 解析得到 `data: {JSON}` 而非 JSON，且 chunk 数包含多余 DONE |
| 同样断言，修复后重跑 | `07b7a09` | **绿灯，exit 0**；`internal/quotaplugin 0.129s` |
| `go vet -mod=readonly ./...` | `07b7a09`，Windows | exit 0 |
| `go test -mod=readonly ./... -count=1` | `07b7a09`，Windows | exit 0；`internal/ollama 1.549s`、`internal/quotaplugin 128.577s` |
| 空流 handler 四项用例 | `07b7a09` 产品代码不变 | **红灯，exit 1**；`internal/quotaplugin 0.141s` |
| 同组流式断言、vet、diff check | 加入空流守卫后 | **绿灯，exit 0**；`internal/quotaplugin 0.257s` |
| 聚焦流式回归 `-count=10` | 最终源码 `fb79c29`，Windows | exit 0；`internal/quotaplugin 0.393s` |
| `gofmt -l` / `git diff --check` | 最终源码 | Go 文件无格式输出，diff check 通过 |
| `go.mod` / `go.sum` 对基线比较 | 最终源码 | 无变化 |
| Mac `go vet` / 全量 `go test -race` | 最终源码 `fb79c29` | exit 0；`internal/ollama 1.627s`、`internal/quotaplugin 129.716s` |
| Mac arm64 c-shared / ELF / Go metadata | 最终源码 `fb79c29` | 构建与两类架构校验通过 |
| 真实 CPA + HTTPS fixture | 最终源码 `fb79c29` | 原 driver 15/15、canary 与资源比较通过；环境限制另列 |

模拟宿主封帧后按标准 SSE **仅去一层**字段前缀解析，覆盖 3/40-token 数量与顺序、stop、usage、唯一 DONE、CRLF、多行 data、注释与元数据、重复/缺失上游 DONE、无尾部分隔符、JSON 回退；正文里的字面量 `data: [DONE]` 不当成控制字段剥除。

首次修复前的代表性原始失败：

```text
TestExecuteStreamReturnsDataForHostFraming:
  got 3 chunks, want 2
TestExecuteStreamWrapsANonStreamingAnswer:
  got 2 chunks, want 1
TestExecuteStreamHostFramingPreservesContent/3_tokens:
  host-framed event is not JSON after removing one SSE prefix:
  "data: {\"choices\":[{\"delta\":{\"content\":\"T01 \"},\"finish_reason\":null}]}"
```

完整红灯与绿灯输出保存在未入库的 `.agent-进度.md`；下述 Mac 原始证据另外保留，不用本报告文字替代日志。

### 空流回归防护

移除原先无条件追加的 DONE 后，空数据由至少一个同步 chunk 变为零 chunk。锁定 SDK 仅在 `len(resp.Chunks) > 0` 时返回预填充的同步关闭通道；零 chunk 会转入本插件没有实现的异步 emit 协议。这是本次改动直接引入的回归，经跟进批准纳入本票。

新增 handler 用例覆盖空 200 event-stream body、空 200 JSON body、注释-only、DONE-only。先保持 `07b7a09` 产品代码不变，四项均失败并返回：

```json
{"ok":true,"result":{"headers":{"Content-Type":["text/event-stream"]},"chunks":[]}}
```

随后只在 `handleExecutorExecuteStream` 中增加零 chunk 守卫，复用 `executorFailure(ollama.ErrParse, credential)`；四项都要求错误码 `upstream_invalid_response`、HTTP 502、既有文案 `the Ollama upstream sent a response this plugin could not read`，并检查错误响应不含 fixture 凭据。没有新增错误码，也没有修改 `sseFrames` 签名、SDK 或缓冲方式。

```sh
ASSISTANT_NO_SEND=1 go test -mod=readonly ./internal/quotaplugin \
  -run '^(TestExecuteStream|TestSSEFrames)' -count=10
ASSISTANT_NO_SEND=1 go vet -mod=readonly ./...
ASSISTANT_NO_SEND=1 go test -mod=readonly ./... -count=1
```

变更路径自查：没有新网络目的地、新凭据处理、新 goroutine 或循环 IO；原上游 32MiB body 上限保留。不是性能优化，不宣称降低首字节等待。

## Mac 复验方法与准确源码

经跟进批准，直接只读复用 T10 提交 `96ecdf9` 的四份工具，不修改 T10 工作区、标准 SSE parser、网络拓扑或任何业务断言。两轮前均核对了相同 hash：

| 工具 | SHA256 |
|---|---|
| `scheme-b-fixture.go` | `3e5fd1d51d435ab39dc292ff2a90cb8c2b73bb161f2b2ec7d551b1f55e6b7a68` |
| `verify-scheme-b.mjs` | `077e1259de236b0b87b0ae5ae84a95017e91d6cf039a82e9bbfad4010a45a99f` |
| `verify-scheme-b-mac.sh` | `b1a24cf5ed0dc2239a1a4a7a5a5ab0a2b2f6b31c6c528805c9d6b422699c669a` |
| `scan-scheme-b-logs.mjs` | `7d5c2c1d07520540c60b6ae9a897654abde3edad4fd4ba12d08fdb8a237d8afd` |

准确源码通过 `git archive` 导出；两份归档本地和 Mac SHA256 一致，第一轮文件不覆盖：

| 轮次 | 源码 | 源码归档 SHA256 |
|---|---|---|
| 第一轮 | `07b7a09` | `a3b75ba3df7950baf72f7e9929682cb9f84688e9bbcc49bfa698b7d35f9c79e7` |
| 最终轮 | `fb79c29` | `d135b7e833ea632053ee99ef34ac22a6deb7618b3981c384a73bce399111fa4b` |

Mac 独立根目录：`~/Projects/cpa-hotplug-test/t13-double-prefix.pe1lRa`。最终轮明确清空 `T10_PRIOR_TEST_RUN`，重新 vet/race 与构建，不能把第一轮 `.so` 当成最终产物。

CPA 镜像：`cpa-hotplug-test:v8.0.47`，image ID `sha256:ba896394d4a291e11f76e205d3469f47996c19b1a78309a4480a0406312279cf`，平台 `linux/arm64`。镜像 tag 仅作来源记录；实际启动日志为 `CLIProxyAPI Version: dev, Commit: none, BuiltAt: unknown`，不宣称已由该日志证明精确宿主源码版本。

- 两轮各自新建 source、build、config、auths、plugins、evidence、internal 网络；不挂载真实 auth、不覆盖旧 dist。
- fixture 内存生成 TLS 私钥，只落公钥证书；CPA 单独信任证书，固定 `ollama.com` 在隔离网络解析到 fixture，不关闭 TLS 校验。
- 真正验证路径为容器内 `http://cpa:18317`；宿主 loopback 的 internal 网络限制单列环境项，保留原始失败，不修改拓扑来凑绿。
- 短流 3 token，宽松长流 40 token、65 秒 idle budget；两者均检查可解析 JSON、token 顺序与数量、stop、usage、唯一 DONE。同一长流的 5 秒 idle budget 仍应超时，不能当作完成推理。
- runner 沿用 T10 的容器标签和命名前缀，实际归属须结合 T13 根目录、源码 SHA 和资源清单，不能只凭 T10 标签清理。

复跑时使用已经批准的 Mac 与四份工具，先核对 hash，在新目录中运行，不覆盖本轮证据：

```sh
# SOURCE_SHA 是准确代码提交；SOURCE.tar 是对应 git archive，并须两端核对摘要。
# T13_ROOT 是 Mac 上新建的空目录；T10_TOOLS 是上述已核 hash 的工具目录。
ASSISTANT_NO_SEND=1 T10_PRIOR_TEST_RUN= T10_RUN_BASE="$T13_ROOT" \
  bash "$T10_TOOLS/verify-scheme-b-mac.sh" "$T13_ROOT/SOURCE.tar" "$SOURCE_SHA"
```

## 实际 Mac 结果

### 第一轮：仅对应 `07b7a09`

Run：`t13-double-prefix.pe1lRa/t10-scheme-b.sPtFlg`，runner exit 0。Go `1.26.10 linux/arm64`、GCC `12.2.0`；vet 与全量 race 通过（`internal/ollama 1.622s`、`internal/quotaplugin 129.695s`）。c-shared 产物 SHA256：`e84be2c7d1cf0d7b13d711b2cc3c2713c4af8247fa82730e9aab36d0415b3e09`，ELF aarch64 与 Go build metadata 双校验通过。

原 driver 15/15 项通过：短流 200 / 963 bytes / 首数据 3014.196ms / 总时长 3015.186ms；宽松长流 200 / 8032 bytes / 首数据 40220.291ms / 总时长 40222.786ms。严格 5 秒对照在 5015.107ms 超时、0 bytes、status=null，保留整段缓冲行为。

八文件凭据扫描、CPA identity、原 dist checksum、原有容器 identity 比较均 exit 0；宿主 loopback curl exit 7 单列环境项。原始证据本地另存 `dist/t13-evidence-07b7a09/`，不入库。

### 最终轮：对应 `fb79c29`

Run：`t13-double-prefix.pe1lRa/t10-scheme-b.tDRu9h`，runner **exit 0**。`evidence/source-sha.txt` 确认为 `fb79c292ffa70f877a88fa231067f35a9bc1160a`，归档摘要与上述最终轮一致。没有复用第一轮测试或产物。

本轮重新执行：

```text
go version go1.26.10 linux/arm64
gcc (Debian 12.2.0-14+deb12u1) 12.2.0
?  github.com/LambdaTheory/cpa-plugin-ollama-quota/cmd/plugin [no test files]
ok github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/ollama 1.627s
ok github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/quotaplugin 129.716s
```

`go vet -mod=readonly ./...` 与 `go test -mod=readonly -race -count=1 ./...` 均通过。新构建的 `build/ollama-quota-v0.2.0.so` SHA256：**`699bb1bd3bd5f657ab8e91044ea74292f002dc76445264381f0452035bd98542`**。Mac `/usr/bin/file` 确认 ELF 64-bit LSB shared object / ARM aarch64；Go metadata 确认 `-buildmode=c-shared`、`CGO_ENABLED=1`、`GOARCH=arm64`、`GOOS=linux`。不是 Windows DLL，也不是第一轮 `.so`。

原 driver **15/15** 项通过：TLS 验证、禁用到实际启用/注册、quota provider、模型发现、唯一文件凭据、后台与手动额度、非流式推理、短流/长流内容与时序、严格预算对照、上游鉴权与固定路径。标准 SSE parser 没有改成剥两层前缀；两条成功流均完整收到各 token、stop、usage 和唯一 DONE。

| 用例 | HTTP | bytes | 首数据 ms | 总时长 ms | 结果 |
|---|---:|---:|---:|---:|---|
| 短流，3 token，15 秒 idle budget | 200 | 963 | 3021.060126 | 3022.706876 | JSON / 顺序 / 数量 / stop / usage / 单 DONE 通过 |
| 宽松长流，40 token，65 秒 idle budget | 200 | 8032 | 40259.312060 | 40262.482102 | 同上，内容完整 |
| 严格长流，40 token，5 秒 idle budget | null | 0 | null | 5015.490627 | `socket idle timeout after 5000ms`，预期保留行为 |

严格预算没有完成推理；“对照符合预期”不等于“这条流成功”。上游早已发出首帧，但客户端首数据仍等整段生成完才出现，证明本票没有实现真正增量，也不能解决该首字节超时。

原始 `runtime-exits.txt`：

```text
driver_exit=0
credential_scan_exit=0
mac_loopback_curl_exit=7
cpa_identity_compare_exit=0
old_dist_compare_exit=0
untouched_container_compare_exit=0
```

八文件 canary 扫描通过：`build.log`、`cpa.log`、`fixture.log`、`driver.log`、`result.json`、`short_stream.sse`、`long_wide.sse`、`long_strict.sse`。CPA 比较前后容器 ID、PID **49287**、StartedAt、restarts 均一致；原 dist 的五份 `.so` 校验和及原有 `omc-release91-verify-1` identity 比较均 exit 0。这只是首次启用不重启的证据，不是 AC-5 的在途跨版本替换。

宿主 loopback 的原始失败保留为环境项，不改拓扑、不当通过：

```text
curl: (7) Failed to connect to 127.0.0.1 port 18317 after 0 ms: Couldn't connect to server
```

原始证据留在该 Mac run 的 `evidence/`；26 份选定证据（不含 auth/config）本地另存 **`dist/t13-evidence-fb79c29/`**，不入库。含源码/归档/工具摘要、构建及架构输出、driver/result、原始 SSE、canary、资源/退出码、前后 identity/checksum 和 loopback stderr。完整 build 与运行目录保留在 Mac，不覆盖原 dist。

## 保留资源与清理边界

两轮共用本票独立根目录，源码归档、manifest、run/source/build、`.so`、公钥证书、原始 SSE、日志、假配置和容器/网络全部保留；只 stop 本票容器，不删除。Windows 的源码归档、manifest 与证据目录在忽略的 `dist/` 中，`.agent-进度.md` 保持未跟踪，未为此修改 `.gitignore`。

第一轮保留 `cpa-t10-t10-scheme-b-sPtFlg-{build,fixture,cpa,driver,logscan}` 五个已退出容器和 internal 网络 `cpa-t10-t10-scheme-b-sPtFlg-net`，完整 ID 在该轮 `evidence/resources.txt` / `final-container-states.txt`。fixture 停止后记录 exit 2，其他四个 exit 0；不把“全部已停止”写成“全部退出码为零”，也不把退出阶段的状态替代已完成的 driver 结果。

最终轮保留资源如下，五个容器均为 `exited`；fixture 停止后同样记录 exit 2，不宣称优雅退出或零退出码。其余四个 exit 0，driver/scan 在 stop 前已经成功完成；没有为改变退出码修改 fixture/runner。

| 最终轮容器 | ID 前 12 位 | 停止后 exit |
|---|---|---:|
| `cpa-t10-t10-scheme-b-tDRu9h-build` | `ca88863f40ce` | 0 |
| `cpa-t10-t10-scheme-b-tDRu9h-fixture` | `a4c0e6790cb9` | 2 |
| `cpa-t10-t10-scheme-b-tDRu9h-cpa` | `cb99cb453bca` | 0 |
| `cpa-t10-t10-scheme-b-tDRu9h-driver` | `cff3cad81617` | 0 |
| `cpa-t10-t10-scheme-b-tDRu9h-logscan` | `b6bc0599a8a9` | 0 |

最终轮 internal 网络：`cpa-t10-t10-scheme-b-tDRu9h-net`，ID `e3f7903a7e248e01ebb8c2da37c5ab626d59bdd8114d1c3c923baf430c3efbc1`。第一轮和最终轮合计保留 **10 个已退出容器、2 个 internal 网络、2 个 run 目录、两份源码归档及 manifest**；不包含原 T10 的独立资源。

后续清理须另获授权，再按每轮 `evidence/resources.txt`、`final-container-states.txt`、`network.txt` 逐个核实完整 ID、挂载路径与停止状态，单独删除获准的容器/网络，再清理获准的 run/归档/本地证据。不得使用 prune 或按 T10 标签批量删除，不能动 T10 其他 run、原 dist 和现役容器。本票没有执行这些删除。

## 未验与交接

- 真实 Ollama 账号 AC-2：未验，不能用 fixture 余额冒充真实账号验收。
- 在途跨版本热替换 AC-5：本票未重新验证；首次启用的 PID 比较不替代该标准。
- 整段缓冲、首字节超时：保留，另票处理，不因双前缀修好宣称真正增量。
- 空流四种输入由 handler 回归覆盖；未扩写不变的 T10 fixture/driver 来单独构造真实 CPA 空流用例。
- 没有界面，不需要体验环境。没有 push、合并、发布、生产操作或删除资源。
