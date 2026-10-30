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

## 构建

本插件编译成 CPA 的动态库插件（`-buildmode=c-shared`）。本机没有 Go，用 Docker 编译：

```sh
scripts/build.sh                    # linux/amd64 -> dist/ollama-quota-v0.1.0.so
scripts/build.sh --target darwin    # macOS     -> dist/ollama-quota-v0.1.0.dylib
scripts/build.sh -o dist/foo.so     # 指定输出路径
scripts/build.sh --go               # PATH 上有 Go 1.26 时直接本机编译
```

产物名里的 `-v<版本>` 不是装饰：CPA 靠文件名里的版本号判断"这是新版本"，
没有版本后缀的库会被当成同一个文件，不会触发热替换。

## 怎么装进 CPA 测试

1. 把 `dist/` 里的库放进 CPA 的插件目录（路径相对于 CPA 的工作目录）：

   ```sh
   cp dist/ollama-quota-v0.1.0.so /path/to/cpa/plugins/
   ```

2. 在 CPA 的 `config.yaml` 里打开插件并允许本插件：

   ```yaml
   plugins:
     enabled: true            # 插件总开关，不开就一个都不加载
     dir: "plugins"           # 相对 CPA 工作目录，也可以写绝对路径
     configs:
       ollama-quota:
         enabled: true        # 单个插件的开关
         priority: 1
         interval_seconds: 900
   ```

3. 重启一次 CPA（首次装插件要重启；之后换版本不用）。启动日志里应该出现：

   ```
   pluginhost: plugin loaded plugin_id=ollama-quota version=0.1.0
   pluginhost: plugin registered plugin_id=ollama-quota ...
   ```

4. 打开管理界面的凭证额度页，或者直接问接口（`<mgmt-key>` 是 `management.secret-key`）：

   ```sh
   curl -H "Authorization: Bearer <mgmt-key>" http://127.0.0.1:8317/v0/management/plugins
   curl -H "Authorization: Bearer <mgmt-key>" http://127.0.0.1:8317/v0/management/quota/providers
   ```

   第一条里应看到 `"id": "ollama-quota"`、`"registered": true`、`"quota_provider": "ollama"`；
   第二条的 `providers` 里应有 `ollama`。

5. 卸载：`DELETE /v0/management/plugins/ollama-quota`。CPA 会先 `plugin.quiesce` 再卸库，
   进程不重启；之后的额度页里不再有 Ollama 行。

> 当前版本 0.1.0 是骨架：额度数字是固定样例（一个 5 小时窗口、已用 50%、余额 $10.00），
> 真实采集在 #2 补。`POST /v0/management/quota/fetch` 返回样例数据，`reset` 一律返回"不支持"。

## 文档

- [需求 SPEC](docs/spec.md)
- [编排配置](docs/agents/orchestration.md)

## 许可

待定：omc 的 ollama 解析思路源自 LGPL-3.0-only 的上游项目，复用前需要核对许可证义务。
见 SPEC 的「许可」一节。
