# copilot-quota

CLIProxyAPI 插件：把 GitHub Copilot 的额度接进 CPA 原生的凭证额度页。

## 它做什么

CPA 的 `QuotaProvider` 接口允许插件为某类凭证提供额度。本插件接管
`provider == "copilot"` 的凭证（由 GitHub Copilot 登录插件写入），读取每个账号的套餐和剩余额度，
按 CPA 的额度模型返回：

- 套餐名（Free / Pro / Pro+ / Business / Enterprise / Edu）
- 每个额度桶的剩余比例、剩余数量、重置时间
- 主额度桶的「剩余 / 已用 / 总额」摘要
- 采集失败时保留上一次成功的数字并标注「已过期」；从未成功过则只报错误，不造数字

## 数据来源

唯一上游是 Copilot 客户端使用的内部接口：

```
GET https://api.github.com/copilot_internal/user
Authorization: Bearer <github_access_token>
```

它**不是** GitHub 公开文档里的 REST 接口，返回形状可能变化。解析器按
「只认明确形状」处理：认不出就报 `error`，不返回 0 或估算值。

已知的两种形状：

- 付费账号：`quota_snapshots.<bucket>.{entitlement, remaining, percent_remaining, unlimited}`
- 免费账号：`limited_user_quotas`（剩余）+ `monthly_quotas`（总量）

套餐来自 `access_type_sku`（映射表取自 axonhub 2026-09 的实现），缺失时回退到 `copilot_plan`。

## 凭证从哪来

不自己存凭证，也不做 OAuth。读取 CPA 凭证文件里已有的字段（由
[`syepes/CLIProxyAPI-plugin-github-copilot`](https://github.com/syepes/CLIProxyAPI-plugin-github-copilot) 写入）：

```json
{"type":"copilot","github_access_token":"...","github_login":"..."}
```

只读 `github_access_token`；`github_refresh_token` 不碰（刷新是登录插件的事，宿主每次采集都会重新读文件）。
带 `github_base_url` 指向 GitHub Enterprise 的凭证会被拒绝，**不会**把企业 token 发到 api.github.com。

## 派发与共存

插件的 `Identifier()` 是 `copilot-quota`，`DescribeQuota` 返回
`SupportedProviders: ["copilot"]`。宿主因此通过第二段匹配接管 copilot 凭证，而不是和登录插件
抢第一段（`AuthProvider.Identifier() == "copilot"`）。

已知风险：如果登录插件将来自己实现 QuotaProvider，它会在第一段直接命中，本插件会被静默顶掉。
上线后用 `GET /v0/management/credentials/quota/providers` 核对；登录插件升级时重跑。

## 构建

```sh
scripts/build.sh                 # Linux amd64 .so（Docker，本机不需要 Go）
scripts/build.sh --go            # 用本机 Go 构建当前平台
```

产物名 `dist/copilot-quota-v<version>.so`。生产是 **arm64**，交叉编译：

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26.10-bookworm sh -c '
  apt-get update -qq &&
  apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross &&
  CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -trimpath -buildmode=c-shared -o dist/copilot-quota-v0.1.0.so ./cmd/plugin'
```

升级插件不需要重启 CPA：换带新版本号的文件名，再发一条
`PATCH /v8/management/config/plugins` 触发重扫。

## 配置

`plugins.configs.copilot-quota.interval_seconds`，默认 900 秒（15 分钟）。

## 测试

```sh
docker run --rm -v "$PWD":/src -w /src \
  -v cpa-go-mod-cache:/go/pkg/mod -v cpa-go-build-cache:/root/.cache/go-build \
  quota-go:1.26-bookworm sh -c 'go vet ./... && go test ./... -race -count=2'
```

ABI 冒烟（验证共享库能按 CPA 的函数表加载、注册、应答 identifier/describe）：

```sh
gcc -O1 -o dist/abi-smoke scripts/abi-smoke.c -ldl
./dist/abi-smoke ./dist/copilot-quota-v0.1.0.so
```

## 不做

- Copilot 的 OAuth 登录（走已有的登录插件）
- 用量历史、图表、计费预估
- 除 copilot 之外的供应商
- 官方 billing 接口的降级路径（内部接口失效时再加）

## 许可

本插件为新实现，只参考了公开项目的可观察行为（接口字段、SKU 映射），没有复制源码文本。
