# cpa-plugin-ollama-quota

CLIProxyAPI 插件：把 Ollama Cloud 的额度与余额接进 CPA 原生的凭证额度页。

## 这个插件做什么

CPA 有一个 `QuotaProvider` 接口，插件实现它就能把某个供应商的额度喂给 CPA 自己的额度页
（`GET /v0/management/quota/providers`、`POST /v0/management/quota/fetch`），
和 Codex、Claude、Antigravity 的额度并排展示。

本插件只做一件事：读 Ollama Cloud 账号的余额和额度窗口，按 CPA 的额度模型返回。

## 硬约束

**首次安装和升级都不需要重启 CPA 进程。** 经隔离容器实测更正：从插件总开关关闭到
首次加载也可以在运行中完成（见下面的安装步骤与实测报告）。之后换版本靠 CPA 的插件热加载，
按文件名里的版本号识别；插件必须能在换版本时安全卸载
（`plugin.quiesce` → 卸载 → 加载新版本 → `plugin.register`），在途的推理请求不受影响。

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

## 构建

本插件编译成 CPA 的动态库插件（`-buildmode=c-shared`）。本机没有 Go，用 Docker 编译 Linux 产物：

```sh
scripts/build.sh                    # linux/amd64 -> dist/ollama-quota-v0.2.0.so
scripts/build.sh -o dist/foo.so     # 指定输出路径
scripts/build.sh --go               # 用本机的 Go 和 C 工具链编译当前平台
```

`-buildmode=c-shared` 需要**目标平台的 C 编译器**。`golang:1.26-bookworm` 镜像里只有 Linux 的，
所以默认只出 Linux `.so`。**macOS `.dylib` 和 Windows `.dll` 这个脚本不提供**——
要在对应平台上装好 Go 和 Xcode CLT / MinGW 后用 `--go` 本机编（`--go` 跟着主机 GOOS 走，
不交叉编译；跨平台 C 工具链本仓库没有配置，也不在 #1 范围内）。

产物名里的 `-v<版本>` 不是装饰：CPA 靠文件名里的版本号判断"这是新版本"，
没有版本后缀的库会被当成同一个文件，不会触发热替换。

## 怎么装进 CPA 测试

下面这份配置在 `CLIProxyAPI v8.0.47` 上验过：插件加载、注册、额度接口都通。

> **先备份 `config.yaml`。** CPA 会改写这个文件：启动时把 `management.secret-key` 换成哈希，
> `DELETE /v0/management/plugins/<id>` 会把 `plugins.configs.<id>` 整段删掉（并重新排版整个文件）。
> 本插件不需要你手动改这份配置之外的任何东西。

1. 把 `dist/` 里的库放进 CPA 的插件目录（路径相对于 CPA 的工作目录）：

   ```sh
   cp dist/ollama-quota-v0.2.0.so /path/to/cpa/plugins/
   ```

2. 准备插件配置，打开总开关并允许本插件。一份完整的 `config.yaml` 例子：

   ```yaml
   config-version: 8

   server:
     host: "0.0.0.0"
     port: 8317

   management:
     allow-remote: true
     secret-key: "换成你自己的"

   plugins:
     enabled: true            # 插件总开关，不开就一个都不加载
     dir: "plugins"           # 相对 CPA 工作目录，也可以写绝对路径
     configs:
       ollama-quota:
         enabled: true        # 单个插件的开关
         priority: 1
         interval_seconds: 900

   oauth:
     auth-dir: "~/.cli-proxy-api"
   ```

   `config-version: 8` 只是标明文件已经是 v8 布局，**不是插件的开关**。
   实测（v8.0.47）：带不带这一行、旧版平铺写法（`port` / `remote-management`）还是 v8 嵌套写法，
   启动后 `plugins.configs` 都保留、插件都正常注册。缺它不会导致插件被卸载。

3. **在运行中开启，不重启。** `.so` 放进 `plugins/` 后，应用上面的配置即可首次加载。
   v8.0.47 的配置写入接口使用 **`/v8/management/config`**：

   ```sh
   curl -X PATCH http://127.0.0.1:8317/v8/management/config \
     -H "Authorization: Bearer <mgmt-key>" \
     -H 'Content-Type: application/json' \
     -d '{"plugins":{"enabled":true,"dir":"plugins","configs":{"ollama-quota":{"enabled":true}}}}'
   ```

   v8.0.47 没有 `PATCH /v0/management/config`（实测 404），v0 的配置入口只有
   `GET /config` 和 `GET/PUT /config.yaml`。备选是直接编辑 `config.yaml`，由 watcher
   捕获并应用（实测的是容器内 `cat` 原位覆盖；**宿主侧编辑 bind mount 文件未验证**，
   可能不触发容器内 watcher）。
   优先使用管理接口。请确认加载日志、注册接口及 **进程 PID 不变、没有重启**，
   详见[首次安装实测](docs/hotswap-verification.md#首次安装从未启用插件子系统到插件生效)。
   运行中的日志里应该出现：

   ```
   pluginhost: plugin loaded plugin_id=ollama-quota version=0.2.0
   pluginhost: plugin registered plugin_id=ollama-quota ...
   ```

4. 打开管理界面的凭证额度页，或者直接问接口（`<mgmt-key>` 是 `management.secret-key`）：

   ```sh
   curl -H "Authorization: Bearer <mgmt-key>" http://127.0.0.1:8317/v0/management/plugins
   curl -H "Authorization: Bearer <mgmt-key>" http://127.0.0.1:8317/v0/management/quota/providers
   curl -H "Authorization: Bearer <mgmt-key>" -H 'Content-Type: application/json' \
        -X POST http://127.0.0.1:8317/v0/management/quota/fetch \
        -d '{"auth_index":"<凭证的 auth_index>"}'
   ```

   第一条里应看到 `"id": "ollama-quota"`、`"registered": true`、`"quota_provider": "ollama"`；
   第二条的 `providers` 里应有 `ollama`。

   额度接口是 `GET /v0/management/quota/providers` 和 `POST /v0/management/quota/fetch`
   （fetch 要 POST + JSON body）。`/v0/management/credentials/quota/providers` 在 v8.0.47 上
   是 404，别用那条。

5. 卸载：`DELETE /v0/management/plugins/ollama-quota`。CPA 会先 `plugin.quiesce` 再卸库，
   进程不重启；之后的额度页里不再有 Ollama 行。**注意这条命令会把 `plugins.configs.ollama-quota`
   从 `config.yaml` 里删掉**，想重新装回来要再把上面那段加回去。

> **`quiesce` 的排空是「有界保证」**：依次判定 collector stops、`Provider.Track` 取数、
> 宿主回调三组工作，最坏等待上界约为「各 collector stop 预算之和 + 5 秒 + 5 秒」。
> 单个默认 collector 的 stop 预算是 20 秒加 2 秒余量，因此总上界约 32 秒；
> **正常情况远小于这个值**，工作已结束就立即继续，不会每次都等满预算。
> - 上限内排空完成 → `plugin.quiesce` 返回成功，库可以安全卸载。
> - 超过上限（宿主没响应取消）→ 返回错误 `quiesce_drain_timeout`，**不是成功**；
>   `detail.in_flight` / `detail.host_pending` / `detail.unsettled` 报告未完成工作。此时卸载
>   会把仍有回调在跑的库换掉。
>
> 为什么要设上限而不是一直等：宿主回调是同步 C 调用，插件无法中断它；无限等会让一次卡死的
> 采集永久堵住热替换。
>
> **宿主侧目前并不因为这个错误而阻止替换**（`internal/pluginhost/host.go:291` 只记日志就继续
> `startPluginLoad`），所以这条保证要靠宿主改动才能变成硬约束——需另立 CPA 票。插件这一侧
> 能做的已经做了：错误可观察、可作判据。
>
> **可以重试，而且重试会重新判定。** 失败后宿主可以再次调用 `plugin.quiesce`/`plugin.shutdown`：
> 只要那次拖住排空的回调还没退出，重复调用**仍然返回同一个错误**；只有观察到它真的退出，
> 才会返回成功。重试不是「按上一次的结论作答」，而是每次都重新做一遍有界排空判定
> （上限同上）。

> 当前版本 0.2.0 已接真实采集：凭证里的 `api_key` 走 `GET https://ollama.com/api/balance`，
> 只有 `cookie` 时走 `GET https://ollama.com/settings` 解析 HTML；两条都失败时返回上一次成功读到的
> 数字并标「已过期」，从没成功过则返回错误、不带数字。插件按 `interval_seconds`（默认 900 秒）
> 定时采集，也能被 CPA 的额度页刷新按钮触发。
>
> `reset` 一律返回"不支持"——Ollama 没有重置额度的接口。
>
> 凭证字段：`{"type":"ollama","api_key":"..."}` 或 `{"type":"ollama","cookie":"..."}`，
> 两者都在凭证 JSON 顶层；两种都配时优先 API Key（见 `internal/quotaplugin/credential.go`）。
>
> **已知未满足的约束（SPEC 4.2）**：要求凭证只发往固定的 `ollama.com`。插件把请求 URL 钉成
> 编译期常量，并拒绝自己看到的任何 3xx；但 CPA 宿主那个 HTTP 客户端**会跟随重定向**
> （`internal/pluginhost/http_bridge.go` 的 `CheckRedirect`），且 `HTTPResponse` 不回传最终 URL。
> Go 的 `http.Client` 在跟随重定向时，对**跨任意域**会删除 `Authorization` / `Cookie`，
> 但对**同域与子域**（`shouldCopyHeaderOnRedirect`，例如 `sub.ollama.com`）**会保留**。
> 所以残余的、插件无法察觉也无法撤销的风险是**子域转发**：凭证可能被送到 ollama.com 的子域。
> 插件无法在事后发现或补救，**这条要改宿主才能根治，需另立 CPA 票**；在宿主修好之前，
> 本插件不满足 SPEC 4.2 的严格要求。

## 推理：一把 key 同时用于额度和推理（0.2.0 起）

0.1.x 只做额度采集。那时 CPA 没有 `ollama` 这个推理 provider，想让 CPA 真正跑 Ollama 推理，
唯一的办法是再配一份 `openai-compatibility` 凭证——那是**第二份**凭证，后台定时采集读的是
`host.auth.list`，看不到这种合成的凭证，额度页因此长期没有数字，两把 key 也容易一把过期一把没过期。

0.2.0 起插件自己声明 `executor` 和 `model_provider`，成为 provider `ollama` 的推理执行者。
现在同一个凭证文件同时干两件事：

```json
{"type":"ollama","api_key":"..."}
```

- `api_key` 既走 `https://ollama.com/v1/chat/completions`（推理），也走 `/api/balance`（额度）；
- 模型清单不写死：每次凭证注册时用这把 key 打 `GET https://ollama.com/api/tags`，拿这个账号
  当前能用的模型（`model.for_auth`）。账号变了模型跟着变，不用等插件发版；
- 只有 `cookie` 的凭证照常报额度，但不能推理（推理只认 API Key）。这不算配置错误：
  插件不给它注册任何模型，也不报错。

已知取舍：

- **流式不是逐字增量。** 插件把上游 SSE 整段读完再切成帧交给宿主（`executor.execute_stream`
  同步返回 `chunks`），所以客户端拿到第一帧的时间等于整段生成结束的时间。这是当前宿主协议里
  同步返回这条路线的形态；要做真增量得走 `host.stream.emit` 异步通道，留作后续。
- **`executor.count_tokens` 直接返回 501。** Ollama 没有 token 计数接口，猜一个数会违反
  「宁缺毋错」（SPEC 4.3），宿主会回落到自己的估算器。
- **`executor.http_request` 只转发到 `https://ollama.com`。** 其它域一律 400，凭证不会被带到
  别家域名。
- **热替换不取消在途推理。** SPEC 4.1 要求在途请求跑完，所以执行调用不登记在 `Provider.Track`
  里；宿主会等它结束，超过 quiesce 的有界预算时如实返回 `quiesce_drain_timeout`。

## 文档

- [需求 SPEC](docs/spec.md)
- [编排配置](docs/agents/orchestration.md)
- [热插拔实测](docs/hotswap-verification.md)——首次安装和升级不重启 CPA 的实测报告，脚本是
  `scripts/verify-first-install.sh` 与 `scripts/verify-hotswap.sh`；热替换安全前置的反向验证是
  `scripts/verify-hotswap-guards.sh`

## 许可

待定：omc 的 ollama 解析思路源自 LGPL-3.0-only 的上游项目，复用前需要核对许可证义务。
见 SPEC 的「许可」一节。
