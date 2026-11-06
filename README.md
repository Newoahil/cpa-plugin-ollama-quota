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

发布版本为 **0.2.1**，正式发布包为 **linux/arm64** 的
`ollama-quota_0.2.1_linux_arm64.zip`，包内为 `ollama-quota-v0.2.1.so`。
下方通用脚本默认使用主机架构；正式发布使用「Linux arm64 发布包」一节的工作流。

本插件编译成 CPA 的动态库插件（`-buildmode=c-shared`）。本机没有 Go，用 Docker 编译 Linux 产物：

```sh
scripts/build.sh                    # linux/amd64 -> dist/ollama-quota-v0.2.1.so
scripts/build.sh -o dist/foo.so     # 指定输出路径
scripts/build.sh --go               # 用本机的 Go 和 C 工具链编译当前平台
```

`-buildmode=c-shared` 需要**目标平台的 C 编译器**。`golang:1.26-bookworm` 镜像里只有 Linux 的，
所以默认只出 Linux `.so`。**macOS `.dylib` 和 Windows `.dll` 这个脚本不提供**——
要在对应平台上装好 Go 和 Xcode CLT / MinGW 后用 `--go` 本机编（`--go` 跟着主机 GOOS 走，
不交叉编译；跨平台 C 工具链本仓库没有配置，也不在 #1 范围内）。

产物名里的 `-v<版本>` 不是装饰：CPA 靠文件名里的版本号判断"这是新版本"，
没有版本后缀的库会被当成同一个文件，不会触发热替换。

## Linux arm64 发布包

生产插件使用 **Linux/arm64**。上面的 `scripts/build.sh` 默认 Docker 构建不保证 arm64，
发布包使用下面的显式交叉编译流程（Bash / Git Bash，在仓库根目录运行）：

```sh
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$(pwd -W 2>/dev/null || pwd):/src" -w /src \
  -e ASSISTANT_NO_SEND=1 -e RELEASE_VERSION=v0.2.1 \
  golang:1.26.10-bookworm bash -euc '
    apt-get update -qq
    apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross python3
    library=$(python3 scripts/package-release.py --version "$RELEASE_VERSION" --check-version)
    CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
      go build -trimpath -buildmode=c-shared -o "dist/$library" ./cmd/plugin
    python3 scripts/package-release.py --version "$RELEASE_VERSION" --library "dist/$library"
    cd dist/release
    sha256sum --check checksums.txt
  '
```

`RELEASE_VERSION` 必须与 `internal/quotaplugin/plugin.go` 的 `Version` 一致，允许带 `v` 前缀。
脚本在编译前检查该版本，打包时再次检查版本和 `.so` 文件名，并读取 ELF 头拒绝非 arm64、
非 ELF64 或非共享库输入。它不加载动态库；二进制内部的注册版本由本次源码构建保证。

产物在 `dist/release/`：

- `ollama-quota_0.2.1_linux_arm64.zip`：根目录只有 `ollama-quota-v0.2.1.so`，
  不含子目录、C 头文件或其他文件；MIT 许可全文保存在 ZIP 注释中。
- `checksums.txt`：ZIP **整体**的 SHA-256，不是 `.so` 的哈希。

已有 arm64 `.so` 时也可直接使用标准库打包脚本：

```sh
ASSISTANT_NO_SEND=1 python3 scripts/package-release.py --version v0.2.1 \
  --library dist/ollama-quota-v0.2.1.so --output-dir dist/release-check
```

每次使用空的输出目录；目录已有内容时脚本会报错，避免覆盖旧包或混用校验和。
将 ZIP 解压到独立目录后检查仅有一个 `.so`，再按下面的安装步骤放进 CPA；
解压文件的 SHA-256 应与构建输入一致。

ZIP 文件名遵循官方商店的 `<id>_<version>_<goos>_<goarch>.zip` 契约，版本不带 `v`；
`checksums.txt` 中使用相同文件名。包内动态库保留 `-v0.2.1` 后缀，供 CPA 识别热升级。

`.github/workflows/release.yml` 支持手动填写版本或匹配 `v*` 的 tag 触发，使用相同的
Bookworm 工具链构建并检查 Linux/arm64 包。工作流只有 `contents: read` 权限，
将 ZIP 与 `checksums.txt` 保存为 Actions artifact `ollama-quota-linux-arm64`；
它不会创建 tag、创建 GitHub Release 或向官方商店提交内容。

## 怎么装进 CPA 测试

### 配置页面（0.1.3 起）

插件启用后，管理台左侧「插件」分组下出现「Ollama 额度」。页面地址为
`/v0/resource/plugins/ollama-quota/settings`，页面不需要鉴权，也不包含任何密钥。
在页面填入 CPA 管理密钥后读取配置，再填写 Ollama API Key 并点击「保存并采集」。
页面显示配置状态、打码尾号、最近成功采集时间和余额；失败会显示原因，旧余额会标记过期。
Ollama API Key 提交后清空输入框，接口不返回明文；重开页面仍能读取已配置状态。

页面选择手动输入管理密钥，每次调用管理接口时带 Bearer；管理密钥存在此浏览器的
localStorage 中，可点击「忘记管理密钥」清除。Ollama API Key 不存浏览器 localStorage。
页面不依赖管理台的私有登录存储或外部 CDN。

在 `plugins.configs.ollama-quota` 下可以配置：

```yaml
interval_seconds: 900
state_dir: /path/to/persistent/ollama-quota
```

`state_dir` 必须指向插件专用目录。目录权限 0700，内部 `settings.json` 权限 0600，
文件只保存页面提交的 API Key。未设置时使用 `os.UserConfigDir()/cpa-plugin-ollama-quota`；
无法取得用户配置目录时退到 `os.UserHomeDir()/cpa-plugin-ollama-quota`。
容器部署应显式把 `state_dir` 指向可写且持久挂载的目录，保证换容器后配置仍在。
不要把这个目录或 settings.json 提交到 Git。

凭证优先级为：页面保存的 Key → CPA 凭证存储（StorageJSON / host.auth.get）→
QuotaFetchRequest.Metadata。沿用已有 CPA 凭证时，无需固定 auths 路径；以运行实例配置为准。
有页面配置时，即使 CPA 没有 Ollama auth 文件，页面刷新和定时采集仍可工作。

管理接口（都经过 CPA 管理鉴权）：

- `GET /v0/management/plugins/ollama-quota/settings`：配置状态与最近结果。
- `POST /v0/management/plugins/ollama-quota/settings`：JSON `{"api_key":"..."}`，保存后立即采集。
- `POST /v0/management/plugins/ollama-quota/refresh`：刷新已配置账号的余额。

以下安装与热更新实测记录针对 **CPA v8.0.47 / 插件 v0.1.2**，不是 v0.2.1 的实测结论。

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

- **流式逐事件增量输出。** 插件通过 `host.http.do_stream` / `host.http.stream_read` 读取上游，
  收到首个完整有效事件即向宿主返回异步流，后续事件通过 `host.stream.emit` 逐个交出，不再等待全文结束。
  SDK Payload 只含裸 JSON；宿主统一封装单层 SSE，并在正常关闭后写唯一的 `[DONE]`。
  任意网络分片、CRLF、多行 data 字段都会先组为完整事件，token、stop、usage 顺序不变。
  空流在返回异步成功前仍报 HTTP 502；中途断开则关闭流并报告错误，客户端取消会传递到上游。
  仍保留总响应 32 MiB 上限；上游若忽略 stream 返回普通 JSON，只能在完整 JSON 到达后回退为一个事件。
- **`executor.count_tokens` 直接返回 501。** Ollama 没有 token 计数接口，猜一个数会违反
  「宁缺毋错」（SPEC 4.3），宿主会回落到自己的估算器。
- **`executor.http_request` 只转发到 `https://ollama.com`。** 其它域一律 400，凭证不会被带到
  别家域名。
- **热替换不取消在途推理。** SPEC 4.1 要求在途请求跑完，所以执行调用不登记在 `Provider.Track`
  里；异步流从 HTTP 预检到最终关闭的整个生命周期计入宿主桥的 in-flight 屏障，而不是只统计瞬时回调。
  超过 quiesce 的有界预算时仍如实返回 `quiesce_drain_timeout`，没有放宽或改变原有卸载策略。

## 文档

- [需求 SPEC](docs/spec.md)
- [编排配置](docs/agents/orchestration.md)
- [热插拔实测](docs/hotswap-verification.md)——首次安装和升级不重启 CPA 的实测报告，脚本是
  `scripts/verify-first-install.sh` 与 `scripts/verify-hotswap.sh`；热替换安全前置的反向验证是
  `scripts/verify-hotswap-guards.sh`

## 许可

待定：omc 的 ollama 解析思路源自 LGPL-3.0-only 的上游项目，复用前需要核对许可证义务。
见 SPEC 的「许可」一节。
