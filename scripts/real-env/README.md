# 真实上游验证环境（T7 三家额度插件）

用真实凭证把 ollama / Command Code / OpenCode Go 的额度接口跑通。与 `scripts/dev-env`
（`cpa-dev-env-t7`，端口 18600，假上游）**完全隔离**：端口、容器名、数据目录都不同，
两边可以同时开着。

| 项 | 值 |
|---|---|
| CPA 端口 | `18700`（仅绑 `127.0.0.1`） |
| 容器名 | `cpa-real-env-t7` |
| 数据目录 | `scripts/real-env/.runtime/`（gitignored） |
| 凭证来源 | 仓库根 `.env.real-keys`（gitignored） |

## 用法

在仓库根执行（Git Bash）：

```sh
bash scripts/real-env/cpa-real-env.sh up       # 起容器、灌真 key、等插件注册
bash scripts/real-env/cpa-real-env.sh verify   # 拉一轮额度 + 凭证泄漏检查
bash scripts/real-env/cpa-real-env.sh status
bash scripts/real-env/cpa-real-env.sh logs
bash scripts/real-env/cpa-real-env.sh down     # 停并删除本容器和数据目录
```

`up` 只读 `.env.real-keys`，**不打印任何 key 值**；缺哪一项就在最后列出变量名。
`.env.real-keys` 是唯一存放真 key 的地方，`.runtime/auths/` 的凭证文件由它生成。

## 凭证文件写法

四条规则来自 `scripts/dev-env/README.md` 的实测结论，这里照做：

- 兼容上游的凭证必须是磁盘 auth 文件，`type` 写带空格的 provider key 原样；
- `api_key` 优先，`cookie` 只在 `type: ollama` 上使用；
- 一个上游一个账号一个文件，额度按凭证分行；
- `base_url` 指向真实上游，不指向桩。
