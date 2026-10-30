# 编排配置

本文件给 orca-issue 编排用。只写已核实的事实和边界。

## 仓库

- GitHub：`LambdaTheory/cpa-plugin-ollama-quota`（私有）
- 本机路径：`E:/works/cpa-plugin-ollama-quota`
- 默认分支：`main`
- 开发主线：`dev`（首次初始化时创建）

## 技术栈

- Go，模块名 `github.com/LambdaTheory/cpa-plugin-ollama-quota`
- 依赖 `github.com/router-for-me/CLIProxyAPI/v8` 的公开 SDK（`sdk/pluginapi`、`sdk/pluginhost`）
- 编译方式：`go build -buildmode=c-shared`，必须 `CGO_ENABLED=1`
- 产出：Linux `.so`、macOS `.dylib`、Windows `.dll`

## 本机构建环境

**本机没有装 Go。** 用 Docker 编译：

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm \
  sh -c 'apt-get update && apt-get install -y --no-install-recommends build-essential && \
         CGO_ENABLED=1 go build -buildmode=c-shared -o dist/ollama-quota.so ./cmd/plugin'
```

上游 CPA 的 Dockerfile 用的就是 `golang:1.26-bookworm` + `build-essential`。

## 参考实现

- CPA 插件 SDK：`E:/works/CLIProxyAPI/sdk/pluginapi/`、`sdk/pluginabi/`、`sdk/pluginhost/`
- 插件样例：`E:/works/CLIProxyAPI/examples/plugin/`（20 余个，含带管理路由和 quota 的）
- 额度接口契约：`E:/works/CLIProxyAPI/sdk/pluginapi/types.go` 的 `QuotaProvider`（约 1553 行起）
- omc 的 ollama 实现：`E:/works/Newoahil__oh-my-cpa/internal/ollama/`（dev 分支，含 `balance.go`、`parse.go`、`collector.go`）
  —— **只读参考，不复制源码**

## 边界

- 不改 CPA 本身（`E:/works/CLIProxyAPI` 只读）
- 不改 omc（`E:/works/Newoahil__oh-my-cpa` 只读）
- 不改注册表上架（需要单独授权）

## 验收约定

- 单测用 `go test ./...`（Docker 里跑）
- 端到端验证需要一个真实 CPA 实例加一条真实 Ollama 凭证；没有凭证时用 fixture 服务器代替，
  并在报告里写明用的是哪种

## 用户节点

浩文要亲自看的：
- AC-5（升级不重启、不断流）必须实测，不能只靠单测
- AC-2（余额与网站一致）需要真实凭证
