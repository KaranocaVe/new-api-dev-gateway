# New API Dev Gateway

在 Linux 开发容器内裸跑 [New API](https://github.com/QuantumNous/new-api)，为开发工具提供供应商聚合、模型路由和故障切换。渠道和密钥由一个私有 JSON 文件注入。

使用官方 Linux 二进制，通过管理 API 设置功能和渠道；不修改上游源码。配置 CLI 初始化后以 `exec` 替换自身，稳定运行时只有 **一个 New API 进程**。SQLite 是唯一数据库，不需要 Redis、MySQL、Node.js、Python 或额外代理。

**部署边界：官方 New API 监听所有接口，配置不能改为仅监听 localhost。请在可信、隔离的开发容器网络中运行，不发布 3000 端口，也不要启用开发平台的公网端口转发。首次初始化的临时端口同样监听所有接口；不可信客户端能够访问时，存在抢先完成 setup 的风险。** 客户端令牌默认仅接受环回 IP，这是令牌访问限制，不能代替网络隔离。需要监听地址级别隔离的环境，不适合直接部署本项目的未修改二进制。

## 安装与启动

支持 Linux **amd64、arm64**，普通用户运行。系统需要 HTTPS 根证书、正常 DNS，以及访问 GitHub Releases 和供应商 API 的网络。

下载已测试的配置 CLI，不需要 Go：

```sh
git clone https://github.com/KaranocaVe/new-api-dev-gateway.git
cd new-api-dev-gateway
sh scripts/install.sh v0.1.0
export PATH="$HOME/.local/bin:$PATH"
```

安装脚本使用明确的 CLI 版本、下载对应架构并核对发布的 `SHA256SUMS`。也可以在有 Go 1.23+ 的环境从源码构建：

```sh
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -trimpath -o "$HOME/.local/bin/new-api-gateway" ./cmd/new-api-gateway
```

准备纯 JSON 配置，把示例中的 `key`、`base_url` 和 `models` 改成实际供应商信息：

```sh
mkdir -p "$HOME/.config/new-api-gateway"
chmod 700 "$HOME/.config/new-api-gateway"
install -m 600 examples/gateway.json "$HOME/.config/new-api-gateway/gateway.json"
"${EDITOR:-vi}" "$HOME/.config/new-api-gateway/gateway.json"
new-api-gateway validate --config "$HOME/.config/new-api-gateway/gateway.json"
new-api-gateway run --config "$HOME/.config/new-api-gateway/gateway.json"
```

首次启动自动下载并验证锁定的官方 New API，生成管理员密码和 `SESSION_SECRET`，初始化 SQLite，然后注入设置、渠道和客户端令牌。再次启动复用相同数据库和身份。`run` 在前台运行，SIGINT/SIGTERM 正常退出；不需要 Web 向导或手动登录。管理页面仍可在 `http://127.0.0.1:3000` 访问。

默认状态目录为 `${XDG_STATE_HOME:-$HOME/.local/state}/new-api-gateway`。也可给所有命令传入相同的 `--state /persistent/path`；开发容器重建时，这个目录和源 JSON 必须位于保留的卷或工作区中。

## 连接开发工具

私有的 `client.json` 包含 `base_url` 和 `api_key`，可直接由支持 JSON 配置的客户端读取。需要环境变量的客户端可以用 `jq` 取值；`jq` 只是以下示例的读取工具，不是服务运行依赖：

```sh
state="${XDG_STATE_HOME:-$HOME/.local/state}/new-api-gateway"
export OPENAI_BASE_URL="$(jq -r .base_url "$state/client.json")"
export OPENAI_API_KEY="$(jq -r .api_key "$state/client.json")"
curl --fail-with-body "$OPENAI_BASE_URL/responses" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.4","input":"Say hello","store":false}'
```

使用原生 `/v1/responses`，不会先转成 Chat Completions。OpenAI 兼容渠道开启请求体透传，不添加标签、命名前缀、参数覆盖或 Chat 转换。请求体透传仍由 New API 进行鉴权、模型检查、路由和响应处理，并非任意 HTTP 透明隧道。

## 配置

```json
{
  "version": 1,
  "port": 3000,
  "retries": 2,
  "client_allow_ips": ["127.0.0.1", "::1"],
  "runtime": {"GOMEMLIMIT": "256MiB"},
  "channels": [
    {
      "id": "primary",
      "name": "Primary",
      "type": "openai",
      "base_url": "https://api.openai.com",
      "key": "YOUR_PROVIDER_KEY",
      "models": ["gpt-5.4"],
      "priority": 10,
      "weight": 1,
      "enabled": true
    }
  ]
}
```

| 字段 | 行为 |
| --- | --- |
| `id` | 本项目稳定标识；更名渠道时保留它，避免创建新渠道 |
| `name` | 控制台显示名称，配置内必须唯一 |
| `type` | `openai` 为普通 OpenAI 兼容渠道；`anthropic` 为原生 Anthropic 渠道 |
| `base_url` | 供应商根地址，可有路径前缀；通常不带末尾 `/v1`，上游会附加请求路径。例如 `/api` 前缀会生成 `/api/v1/responses` |
| `key` | 直接写入私有 JSON；不做环境变量插值，不接收 OAuth 账号 |
| `models` | 明确列出供应商支持的模型 ID，不自动发现或改写 |
| `priority` / `weight` | 先选更高优先级；同级按权重选择，权重默认 1 |
| `enabled` | 默认 true；false 禁用该渠道，仍保留记录 |
| `responses_websocket` | 默认 false；仅供应商和客户端均支持时开启，上游 WebSocket 行为未纳入当前集成测试 |
| `retries` | 失败后重试次数，默认 2；具体可重试错误由 New API 决定，流已开始后不能保证切换 |
| `client_allow_ips` | 客户端令牌允许的 IP/CIDR，默认环回；连接经过可信代理的场景需另行设计 |
| `runtime` | 受支持的资源与超时设置，详见 [配置机制](docs/configuration.md) |

未知字段会报错；配置文件必须是私有文件（`chmod 600`）。至少保留一个启用渠道和模型。`models` 是精确模型列表，调用不存在的模型会被拒绝；Anthropic 渠道使用 `/v1/messages`，不会把它包装成 Responses 服务。

修改 JSON 后同步：

```sh
new-api-gateway apply --config "$HOME/.config/new-api-gateway/gateway.json"
new-api-gateway status --config "$HOME/.config/new-api-gateway/gateway.json"
```

`apply` 支持热更新渠道、密钥、优先级、模型和重试策略。端口和 `runtime` 需要重启；改端口时先停服务，再修改、启动。同步可重复执行；移出 JSON 的已托管渠道会被**禁用而非删除**，未托管渠道不接管。管理设置和托管渠道以 JSON/本项目策略为准，手工改动会在下次同步覆盖。同步通过多个上游 API 完成，不是跨渠道事务；中途失败后修正原因并重新执行即可。

## 实际关闭了什么

- 关闭注册、外部登录、验证码、签到、额度提醒、模型限流、消费明细日志和统计导出。
- 关闭渠道定时测试、模型更新检查、自动禁用/恢复渠道、插件和异步任务轮询；关闭性能监控及磁盘缓存。
- 隐藏绘图、任务及控制台信息面板；关闭 Chat→Responses 转换。
- 设置自用模式，并把注入模型的本地计价设为零，避免开发工具被本地钱包余额阻断；**供应商侧仍正常收费**。
- 使用 SQLite WAL、较小连接池和连接复用池，默认 Go 软内存目标为 256 MiB。

自用模式本身不会关闭额度逻辑。零计价也不删除内部计账代码。前端、管理 API、部分后端路由以及订阅重置、凭据刷新、系统任务调度等 goroutine 仍在官方二进制内；没有把完整 New API 变成专门裁剪过的程序。详细设置、保留功能和源码证据见 [配置机制](docs/configuration.md)。

## 后台运行

没有内置常驻管理进程。开发容器的进程管理器可直接执行上面的 `run` 命令并负责重启；不要把前台命令或 `nohup` 当作容器重建后的自动恢复方案。

如果环境提供 `systemd --user`，可安装可选用户服务：

```sh
mkdir -p "$HOME/.config/systemd/user" "$HOME/.local/state/new-api-gateway"
chmod 700 "$HOME/.local/state/new-api-gateway"
install -m 644 examples/systemd/new-api-gateway.service "$HOME/.config/systemd/user/"
systemctl --user daemon-reload
systemctl --user enable --now new-api-gateway
systemctl --user status new-api-gateway
journalctl --user -u new-api-gateway -f
systemctl --user restart new-api-gateway
systemctl --user stop new-api-gateway
```

示例服务固定使用 `~/.local/state/new-api-gateway`；若此前使用 XDG 自定义路径或 `--state`，先修改 service 为同一目录。该服务异常退出会重启，是否能在登出后继续运行由宿主用户管理器决定。大多数开发容器没有 systemd，请使用开发平台已有的启动/监督机制。

## 更新、备份与恢复

[release.json](release.json) 固定一次可校验的安装结果；自动化每天检查上游发行版，有新兼容版本时创建或更新**同一条 PR**，不会直接改 main。默认跟随 stable 和标准 `alpha.N` / `beta.N` / `rc.N` 发行线，以 SemVer 排序；跳过 `rc.19-i18nfix.2` 这类分支补丁，避免旧分支因字母后缀排到新 RC 前面；仅正式版策略可给更新脚本添加 `--stable-only`，会同时检查 tag 和 GitHub prerelease 标记。新版必须同时提供精确匹配的 Linux amd64/arm64 资产，并通过真实下载、SHA-256 与 ELF 架构检查；缺资产则跳过，不回退。

更新 PR 合并后，维护者发布新的 CLI tag；用户运行安装脚本安装新 CLI。源码用户可以：

```sh
# 先停止服务并按下文备份，再更新。
git pull --ff-only
CGO_ENABLED=0 go build -trimpath -o "$HOME/.local/bin/new-api-gateway" ./cmd/new-api-gateway
new-api-gateway run --config "$HOME/.config/new-api-gateway/gateway.json"
```

启动时只使用 CLI 内嵌的版本与 SHA，不运行时追逐 `latest`。上游启动会执行数据库迁移；升级前备份，降级需要恢复升级前的完整备份，不能假定旧版能打开新版数据库。

停服务并确认进程已退出后，备份**整个状态目录和源 JSON**：

```sh
state="${XDG_STATE_HOME:-$HOME/.local/state}/new-api-gateway"
backup="$HOME/new-api-gateway-backup-$(date +%Y%m%d-%H%M%S)"
umask 077
mkdir -m 700 "$backup"
cp -a "$state" "$backup/state"
cp -p "$HOME/.config/new-api-gateway/gateway.json" "$backup/gateway.json"
```

SQLite 使用 WAL；不要在运行时只复制 `.db` 文件。恢复时保持停止状态，先把现有 state 移到其他目录，再将 `backup/state` 整体复制回原位置，并恢复源 JSON。`state.json`、`client.json`、数据库和配置必须配套；只恢复数据库可能丢失管理员身份和渠道映射。恢复后用配套版本启动并验证请求。

卸载 CLI 或删除仓库不会删除状态和配置。使用 systemd 时先 `disable --now` 并移除 service 文件，再删除 `~/.local/bin/new-api-gateway`。只有明确不再需要数据时才手动删除状态目录和 JSON。

## 故障排查

| 现象 | 检查 |
| --- | --- |
| 无法下载 | GitHub Releases 网络、HTTPS 根证书；SHA 或 ELF 不匹配会拒绝安装，不可绕过 |
| bootstrap 失败 | 私有 `logs/bootstrap.log`；检查是否用了专用 state，是否有其他实例占用、残留 `.env` 或外部修改的管理员密码 |
| 端口已占用 | `ss -ltnp`；修改 JSON 的 `port` 后重启；CLI 不会停止其他进程 |
| 401/403 | 读取配套 `client.json`，检查模型列表、`client_allow_ips` 和 `TRUSTED_PROXIES=none` 的实际来源地址 |
| 404/503 或无可用渠道 | 检查供应商 Responses 支持、`base_url`、模型 ID、优先级和密钥；不要用 Chat 转换掩盖不支持的接口 |
| JSON 修改未生效 | 渠道改动执行 `apply`；runtime 和端口改动重启；确认 config/state 路径一致 |
| 进程锁报错 | 已有实例或同步操作；先确认进程，锁文件是否存在并不代表锁仍被持有，无需删除它 |
| 删除过托管渠道 | 停止后备份，移除 `state.json` 中该 `managed` 映射，再启动重建；不要删除整个身份文件 |
| RSS 超过 256 MiB | `GOMEMLIMIT` 是 Go GC 的软目标，非 RSS 硬限制；大请求、并发和上游缓冲也占内存 |

配置、数据库、`state.json`、`client.json` 和备份包含凭据明文，目录 0700、文件 0600 是访问权限，不是加密。不要提交或上传这些文件。CLI 不输出密钥或上游错误正文，仍应把上游日志视作私有运行数据。

## 验证与许可

CI 对 amd64、arm64 下载并实际运行官方 New API，使用本地模拟供应商验证 Responses/SSE、原始请求体透传、优先级/故障切换、零本地计价、JSON 轮换/禁用/幂等、单进程、进程锁、SIGTERM/重启、SQLite WAL、权限及备份恢复。单元测试覆盖非法配置、错误脱敏、崩溃后创建恢复和更新器的版本/资产/架构/校验边界。没有使用私人供应商账号，也不代表每个供应商扩展都已验证。详见 [Actions](https://github.com/KaranocaVe/new-api-dev-gateway/actions)。

本仓库的配置工具为 [MIT](LICENSE)。New API 二进制单独从官方下载安装，遵守其自身 [AGPL-3.0 及附加条款](https://github.com/QuantumNous/new-api/blob/v1.0.0-rc.48/LICENSE)；本仓库 MIT 不改变上游授权。
