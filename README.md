# Reso

> 把 WorkBuddy(CodeBuddy)、TraeWork、Qoder 的多个账号聚合成一个 **OpenAI 兼容 API**，自带 Web 管理控制台，Docker 一键部署。

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white&style=flat-square)
![API](https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square)
![WebUI](https://img.shields.io/badge/WebUI-React_19_·_Tailwind_4-61DAFB?style=flat-square)
![Deploy](https://img.shields.io/badge/Deploy-Docker_Compose-2496ED?logo=docker&logoColor=white&style=flat-square)

## 项目简介

Reso（wild-work2api）是一个自托管的多渠道账号聚合网关：

- **三渠道聚合**：WorkBuddy(腾讯 CodeBuddy) + TraeWork(字节) + Qoder(阿里)，模型 ID 带渠道前缀路由（`workbuddy/<model>`、`traework/<model>`、`qoder/<model>`）
- **Web 控制台**：浏览器里添加账号（OAuth 授权自动导入）、签到、刷新积分、停用、删除、查看费率与日志——全部自助，无需命令行
- **账号池治理**：粘性路由优先复用同账号以利用会话缓存、每日定时签到领额度、token 保活、冷却状态机
- **OpenAI 兼容**：`/v1/chat/completions`、`/v1/models`，支持流式/非流式，现有 SDK 零改造接入；另含 **`/v1/responses`（Responses API）兼容层**，Codex 等 Responses 协议客户端零配置直连（含 function calling 往返）
- **单二进制**：Go + go:embed，Web 控制台直接打进二进制，无外部依赖

> [!NOTE]
> 本项目衍生自 [rockswang/wild-work](https://github.com/rockswang/wild-work)（MIT），其前身为 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)。在此基础上重写了 Web 控制台（React 19 + Tailwind CSS 4）并增加了 Docker 部署方案。

> [!WARNING]
> 本项目是**非官方**网关，通过逆向上游客户端接口工作。仅供个人学习研究，请遵守各上游平台服务条款，仅使用本人授权的账号。详见[免责声明](#免责声明)。

## 快速开始（Docker，推荐）

```bash
git clone https://github.com/Libra1337/Reso2api.git
cd Reso2api
cp config.example.json config.json   # 编辑 api_key（公网部署必须设置）
docker compose up -d --build
```

启动后：

- **Web 控制台**：`http://<host>:7863/` → 点「添加账号」→ 浏览器完成 OAuth 登录 → 自动导入
- **API**：`http://<host>:7863/v1` + Bearer `api_key`

首次构建会先编译前端（Node 阶段）再编译 Go，产物全部打进镜像。

### 桌面模式（Windows/macOS）

```bash
go build -o reso ./cmd/wild-work && ./reso        # 系统托盘常驻
./reso --no-tray                                        # Linux 无头模式
```

## Web 控制台

| 页面 | 功能 |
|---|---|
| 概览 | 统计卡（账号池/总积分/启用渠道/下次签到）、渠道分布与状态条、需要关注的账号、服务信息、快捷操作 |
| 账号管理 | 账号卡片（积分/UID/签到状态）、单账号签到/刷新/停用/删除/积分明细；**OAuth 登录添加** 与 **JSON 导入** 双通道，支持批量粘贴 |
| API 接入 | Base URL / API Key 查看复制与修改、签到时间编辑、渠道费率表（一键刷新） |
| 运行日志 | 最近 300 行运行日志，自动刷新、自动滚动 |

**添加账号的两种方式**：

- **OAuth 登录**：选择渠道 → 浏览器打开授权页 → 登录 → 面板自动完成导入（签到、入池）
- **JSON 导入**：粘贴 WorkBuddy 桌面端导出的凭证 JSON（`workbuddy-desktop-*.json` 全文），自动换算过期时间并落盘

> [!NOTE]
> 面板登录：在 `config.json` 中设置 `admin_password` 后，控制台与 `/api/*` 管理接口由内置会话鉴权保护（密码登录 → HttpOnly Cookie + CSRF 校验，7 天有效）。**留空 = 面板不鉴权**，仅适合本机桌面模式；公网部署务必设置。

### 反代示例（Caddy，SSE 流式必需 flush）

```caddyfile
api.example.com {
    reverse_proxy 127.0.0.1:7863 {
        flush_interval -1
    }
}
```

## 客户端接入

```
Base URL: http://<host>:7863/v1
API Key:  <config.json 中的 api_key>
```

模型 ID 必须带渠道前缀：

```bash
# 流式对话
curl -N http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"workbuddy/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 模型列表（当前可用模型）
curl http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"
```

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI 兼容补全，流式/非流式 |
| `POST /v1/responses` | Bearer | **Responses API 兼容层**（Codex 等客户端），请求/事件双向翻译，支持 function calling |
| `GET /v1/models` | Bearer | 各渠道可用模型（带前缀） |
| `GET /status` | Bearer | 账号池状态 |
| `GET /healthz` | 无 | 健康检查 |

### Codex 接入（Responses API）

```toml
# ~/.codex/config.toml
[model_providers.reso]
name = "Reso"
base_url = "https://<your-host>/v1"
wire_api = "responses"
env_key = "RESO_API_KEY"
```

```bash
export RESO_API_KEY=<api_key>
codex -c model_provider=reso -m workbuddy/glm-5.3
```

## 配置说明

完整样例见 [`config.example.json`](config.example.json)：

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen.host` / `port` | `127.0.0.1` / `7863` | HTTP 监听（容器内需 `0.0.0.0`） |
| `api_key` | — | 网关鉴权密钥；面板中可运行时修改 |
| `admin_password` | — | 面板登录密码；空 = 面板不鉴权（仅本机使用） |
| `auth_dir` | `./auths` | 账号凭证目录（明文，注意权限） |
| `state_file` | `./data/state.json` | 账号池状态持久化 |
| `cooldown.*` | 见样例 | 硬冷却（余额耗尽）/ 软冷却（频控）/ 连续错误阈值与退避 |
| `schedule.checkin_times` | `["09:00","21:00"]` | 每日签到时间点（本地时区） |
| `schedule.keepalive_hours` | `[22]` | token 保活整点 |
| `upstream.timeout_seconds` | `120` | 短 RPC 超时 |

账号凭证按渠道命名落盘于 `auths/`：`workbuddy-<uid>.json`、`trae-*.json`、`qoder-*.json`。备份 `auths/` 与 `data/` 即可迁移。

## 开发

```bash
# 后端
go build ./... && go vet ./... && go test ./...

# 前端（Web 控制台）
cd webui
pnpm install
pnpm dev              # 开发服务器，/api 代理到 127.0.0.1:7863
pnpm build:embed      # 构建并注入 ../cmd/wild-work/web（go:embed）

# 完整构建（Docker 自动执行上述两步）
docker compose up -d --build
```

### 目录结构

```
cmd/wild-work/     # 入口（daemon + 托盘 + 无头模式）
  web/             # Web 控制台构建产物（go:embed，由 webui/ 生成）
internal/
  app/             # 管理 API（/api/*）+ 面板数据
  server/          # OpenAI 兼容端点 + 模型前缀路由 + 粘性路由
  pool/            # 账号池（状态机/冷却/持久化）
  scheduler/       # 定时签到 + token 保活
  upstream/        # WorkBuddy 上游封装
  traework/ qoder/ # TraeWork / Qoder 上游封装
  auth/ config/ platform/ systray/ provider/
webui/             # Web 控制台源码（React 19 + Tailwind 4 + shadcn 风格，品牌 Reso）
docs/              # 上游逆向笔记
```

## 免责声明

本项目仅供学习和研究使用。上游接口为非公开的逆向接口，不保证可用性与稳定性。使用者需遵守各上游平台服务条款，自行承担使用风险（包括账号封禁等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

[MIT](LICENSE)

## PostgreSQL 存储模式（可选）

默认与以前完全一致：请求日志与防火墙事件写 `data/*.jsonl`（零依赖）。需要 SQL 级查询时可切到 PostgreSQL：

```bash
# 1. 启动 PG（默认 profile 不启动，显式 --profile postgres）
docker compose --profile postgres up -d postgres

# 2. 一次性导入历史 jsonl（防重跑，可安全重复执行）
docker run --rm --network wild-work_default -v "$PWD:/app" -w /app golang:1.25 \
  go run ./cmd/reqlog-import \
  -dsn "postgres://wildwork:wildwork@postgres:5432/wildwork?sslmode=disable" \
  -reqlog /app/data/request_logs.jsonl \
  -firewall /app/data/firewall_events.jsonl

# 3. data/config.json 增加 storage 段并重启
#    "storage": { "mode": "postgres",
#                 "postgres_dsn": "postgres://wildwork:wildwork@postgres:5432/wildwork?sslmode=disable",
#                 "pg_retention_days": 0 }
docker compose up -d
```

切换后：jsonl 停写（保留为冷备）；面板分页走 SQL（真实总数）；`/api/request_logs/page` 支持可选过滤参数 `model`、`uid`、`status_class=ok|err`、`since`、`until`（unix 秒）。直接 psql 排查：

```sql
SELECT date_trunc('hour', t) h, count(*),
       sum(cached_tokens)::float / greatest(sum(in_tokens),1) AS cache_rate
FROM request_logs WHERE t > now() - interval '1 day'
GROUP BY 1 ORDER BY 1 DESC;
```

回滚：`storage.mode` 改回 `file` 重启即恢复 jsonl 追加（PG 里的数据仍在）。`pg_retention_days>0` 时每日自动清理过期行。请求体存档（10% 采样）两种模式都走文件。
