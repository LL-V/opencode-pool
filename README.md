# opencode-pool — CLIProxyAPI 的 OpenCode Go 账号池插件

把多个 OpenCode Go 账号（邮箱 + API key）组成一个池：每个账号的用量/余量/并发/健康状态一目了然，请求自动分派，并发 429 自动换号，限额按重置时间自动恢复。

## 功能

- **账号池管理**：面板添加（账户名+API key）、启停、删除、导入导出
- **会话亲和**：按会话粘住账号；无会话头的客户端按首条 user 消息派生稳定 ID（system 变更不影响、多轮不变），上游 prompt cache 不失效
- **429 分类**：并发上限（`current limit: N`，自动学习 N）≠ 用量限额（rolling/weekly/monthly），不"见 429 就停号"
- **并发 429 换号**：返回瞬时限流信号，宿主立即换下一个 key 重试，下游无感
- **限额自动恢复**：按上游重置时间精确回池，多窗口并行跟踪
- **实时流式**：异步 host.stream 推送，token 实时输出
- **出站伪装**：User-Agent 强制 `claude-cli/2.1.296 (external, cli)`（可配置）
- 白色极简面板：状态点/用量条/倒计时/并发，键盘 j/k/e/r/u/x 操作

## 安装（Docker / Linux）

> CLIProxyAPI 官方分发是 Docker 镜像（linux/amd64、linux/arm64）——本插件发布 linux/amd64 与 linux/arm64 产物（另有 darwin/windows 供本地开发）。

### 1. 添加本仓库为插件商店源

在 CLIProxyAPI 的 `config.yaml` 里加一行（`store-sources` 追加第三方源，不影响官方源）：

```yaml
plugins:
  store-sources:
    - https://raw.githubusercontent.com/LL-V/opencode-pool/main/registry.json
```

### 2. 面板一键安装

管理面板 → 插件商店 → 找到 **OpenCode Pool** → 安装。

宿主会自动下载与当前平台匹配的产物（如 `opencode-pool_0.1.0_linux_amd64.zip`），校验 sha256 后装入 `plugins/linux/amd64/`，无需重启（宿主热加载插件目录）。

或用 API：

```bash
curl -X POST http://127.0.0.1:8317/v8/plugins/store/opencode-pool/install \
  -H "Authorization: Bearer <管理密钥>"
```

### 3. 添加账号

面板 → OpenCode 菜单 → 添加：填 **账户名（邮箱）+ API key**。

## 配置（可选）

| 字段 | 默认 | 说明 |
|---|---|---|
| `upstream_user_agent` | `claude-cli/2.1.296 (external, cli)` | 出站 UA |
| `near_limit_threshold_pct` | `8` | 距满额多少百分比视为"接近上限" |
| `quota_probe_enabled` | `true` | 窗口重置后探测确认 |
| `panel_poll_seconds` | `5` | 面板刷新间隔 |

也可在面板 Settings 端点在线修改（`GET/PUT /v0/management/plugins/opencode-pool/settings`），修改即持久化。

## 上游

默认 Go 套餐 base `https://opencode.ai/inference/go`，按 API 自动拼接 `/openai/v1/chat/completions`、`/openai/v1/responses`、`/anthropic/v1/messages`（官方 2026-11 文档；旧 `/zen/go` base 会自动迁移）。账号数据独立按 key 存储在插件数据目录，重启不丢。

## 本地开发

```bash
go test -race ./...                 # 全量测试
go build -buildmode=c-shared ...    # 见 Makefile
```

插件 ID `opencode-pool`，模型命名空间 `opencode-go/*`。

## License

MIT
