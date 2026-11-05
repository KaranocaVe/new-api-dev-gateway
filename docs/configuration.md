# 配置机制与边界

锁定版本：`v1.0.0-rc.48`，源码 commit `706144769e5bed5a47b78721e3bb11da3cf4c0b9`。本文记录在该版本上核验的行为；每次跟随上游升级都要通过集成测试。

## 启动过程

1. `run` 持有 `serve.lock`，串行获取 `sync.lock`，验证或下载官方二进制。
2. 在私有 `runtime/` 工作目录临时启动 New API，使用随机高位端口。随机端口用于避免与正式端口冲突，不提供安全隔离。
3. 根据 `/api/setup` 状态初始化专用数据库，使用随机管理员密码登录，经管理 API 同步选项、渠道、客户端令牌，然后注销。
4. 停止临时实例，`exec` 成正式 New API。PID 保持不变，`serve.lock` 文件描述符跨 exec 保留，避免本工具同时启动两个实例。直接手动启动官方二进制会绕过该锁，不受支持。

稳定运行只有一个服务进程。启动期间会短暂存在 CLI 和 bootstrap 子进程。`apply` 是临时管理客户端，不常驻。官方服务内部仍有后台 goroutine；资源限制不能把它们从二进制删除。

`init` 完成上述配置后停止，适合预初始化；不启动常驻服务。`install` 只下载与核验，无需 JSON。`validate` 在 macOS 也可运行；部署命令只允许 Linux。

## 持久化

```text
state/
├── state.json             # 随机身份、secret、令牌ID、托管映射、创建恢复意图
├── client.json            # 开发工具使用的 base_url/api_key
├── serve.lock
├── sync.lock
├── bin/new-api-<tag>-<arch>
├── data/new-api.db         # WAL/SHM 与数据库同目录
├── logs/bootstrap.log
├── logs/...               # 上游日志
└── runtime/               # 独立工作目录，禁止 .env
```

上游默认数据库名是 `one-api.db`；本工具通过绝对 `SQLITE_PATH` 显式使用 `data/new-api.db`，保留 `busy_timeout(30000)`、WAL、`_txlock=immediate` 参数。状态目录不随二进制版本变化。

配置文件权限必须禁止 group/other 读取。创建目录模式 0700，原子写入的 JSON 为 0600，启动 umask 077。配置与状态必须由运行服务的用户拥有；不要用 root 启动一次后再换普通用户。

## 固定环境

完整实现见 [runtime.go](../runtime.go)。外部同名变量会被以下策略覆盖：

| 类别 | 设置 |
| --- | --- |
| 身份/存储 | `SESSION_SECRET`、`CRYPTO_SECRET` 使用持久化随机值；绝对 `SQLITE_PATH`；清空 `SQL_DSN`、`LOG_SQL_DSN`、`REDIS_CONN_STRING`；`NODE_TYPE=master` 允许迁移 |
| 运行 | `PORT` 来自 JSON；`GIN_MODE=release`；`DEBUG=false`、`ENABLE_PPROF=false` |
| 工作量 | `MEMORY_CACHE_ENABLED=false`、`BATCH_UPDATE_ENABLED=false`、`ERROR_LOG_ENABLED=false`、`UPDATE_TASK=false`、`TASK_PLUGIN_ENABLED=false`、`CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED=false`，不设置渠道定时更新频率 |
| 令牌/登录 | `GENERATE_DEFAULT_TOKEN=false`；本地管理 API 登录使用 `PASSWORD_LOGIN_ENCRYPTION_ENABLED=false`，随机密码仅经容器内 HTTP 传递 |
| 连接 | `TLS_INSECURE_SKIP_VERIFY=false`；`TRUSTED_PROXIES=none` |
| 估算/统计 | `CountToken=false`、`GET_MEDIA_TOKEN=false`、`GET_MEDIA_TOKEN_NOT_STREAM=false`；不注入 Umami/Google Analytics |

其余环境（如代理、系统证书位置、时区）仍正常继承。服务在独立 cwd 中读取 `.env` 的上游行为没有改动，因此拒绝该 cwd 出现 `.env`，避免另一套配置混入。本项目的渠道配置来自 JSON，不解析 env 引用。

## 可覆盖的 runtime

JSON `runtime` 是字符串到字符串的白名单。以下默认值可按开发工具请求大小和并发调整，修改后重启：

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `GOMEMLIMIT` | `256MiB` | Go 软内存目标；支持 B/KiB/MiB/GiB 或 off，不是硬 RSS 限制 |
| `GOGC` | `100` | Go GC 增长目标 |
| `SQL_MAX_IDLE_CONNS` | `1` | SQLite 空闲连接数 |
| `SQL_MAX_OPEN_CONNS` | `4` | SQLite 最大打开连接数，必须大于 0 |
| `SQL_MAX_LIFETIME` | `0` | SQL 连接最大寿命，秒，0 不限 |
| `RELAY_MAX_IDLE_CONNS` | `32` | HTTP 空闲连接总数 |
| `RELAY_MAX_IDLE_CONNS_PER_HOST` | `8` | 每供应商空闲连接数 |
| `RELAY_IDLE_CONN_TIMEOUT` | `90` | 空闲连接超时，秒 |
| `RELAY_TIMEOUT` | `0` | 上游请求总超时，秒，0 不限，允许长 Responses 流 |
| `RELAY_RESPONSE_HEADER_TIMEOUT` | `1800` | 等待上游响应头，秒 |
| `STREAMING_TIMEOUT` | `300` | 流读取超时，秒，必须大于 0 |
| `MAX_REQUEST_BODY_MB` | `32` | 入站请求体上限，MiB，必须大于 0 |
| `STREAM_SCANNER_MAX_BUFFER_MB` | `32` | SSE 扫描缓冲上限，MiB，必须大于 0 |
| `SHUTDOWN_TIMEOUT_SECONDS` | `30` | 收到退出信号后等待请求结束，秒 |

32 MiB 可能小于大型代理的长上下文/附件请求，遇到上限时提高对应值并评估内存。请求重试可能重复供应商调用，不对失败请求承诺 exactly-once。

## 管理 API 设置

完整列表由 [api.go: leanOptions](../api.go) 定义。同步前检查上游是否暴露每个必要键，避免把不存在的键静默写进 options 表。只更新变化值。

- 注册、外部身份登录、邮件验证、验证码关闭；保留密码登录供配置同步和管理使用。
- 绘图/任务开关主要控制 UI，不能据此声称相关后端接口都已移除。
- 消费日志、统计导出、敏感词处理、签到、模型请求限流、额度提醒、自动测试和渠道自动禁用/恢复关闭。
- 插件和异步任务轮询通过环境/选项关闭；上游订阅重置、Codex 凭据刷新和系统任务 runner 仍存在，没有对应通用关闭键。
- 性能监控、磁盘缓存及控制台信息面板关闭；内嵌前端仍随官方二进制分发。
- `SelfUseModeEnabled=true`；`ModelPrice`、`ModelRatio` 对启用模型设 0；`billing_setting.billing_mode` 显式设为 `ratio`，避免内置阶梯计价优先；`GroupRatio.default=0`；`quota_setting.enable_free_model_pre_consume=false`。这只使本地计价为零，不影响供应商费用。
- `global.chat_completions_to_responses_policy.enabled=false`；普通 OpenAI 兼容渠道的 `setting.pass_through_body_enabled=true`。不全局启用任意格式透传，不修改模型名。

工具创建一个不设到期时间、无限本地额度、仅允许注入启用模型的客户端令牌。客户端令牌不能访问管理员 API；管理员身份随机生成并保存在 `state.json`。管理 API 本身仍开放于同一个监听端口。

## 源码依据

以下链接指向实际审查过的固定版本：

- [启动、监听、前端嵌入和退出信号](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/main.go)：监听 `":" + port`，没有 bind-address 选项；HTTP listener 启动 100ms 后注册信号处理。
- [Linux 发布构建和命名](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/.github/workflows/release.yml)：分别构建 `new-api-<tag>`、`new-api-arm64-<tag>`，前端先构建后嵌入；发布 `checksums-linux.txt`。
- [环境初始化](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/common/init.go)、[SQLite 默认路径](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/common/database.go)。
- [首次设置](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/controller/setup.go)、[管理设置](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/model/option.go)、[渠道 API](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/controller/channel.go)、[令牌 API](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/controller/token.go)。
- [计价](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/relay/helper/price.go)、[阶梯计价模式](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/setting/billing_setting/tiered_billing.go)、[免费模型预扣设置](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/setting/operation_setting/quota_setting.go)。

## 自动更新的信任边界

更新器不运行下载的上游代码，先完成两个架构的下载、官方校验和/Release digest 核对和 ELF machine 检查，再原子写 release.json。更新只创建 PR；集成测试运行官方二进制，不接触任何私人渠道。

更新 workflow 使用单一固定分支、concurrency 和带 lease 的更新避免重复 PR 与并发覆盖。checkout 不保留凭据，最后推送步骤才配置 Git 认证。因为 GITHUB_TOKEN 创建的 PR 不触发普通 PR CI，workflow 显式 dispatch 该分支的只读 CI。仓库需要允许 Actions 创建 PR；未允许时工作流会明确失败，不会退回直接写 main。
