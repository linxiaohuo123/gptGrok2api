# GPT2API Go

GPT2API Go 是一个自托管的 OpenAI 兼容网关。它使用 Go 运行时接入 ChatGPT/OpenAI JWT 账号池，并提供文本、图片、账号调度、代理、文件存储、实时监控和 Web 管理控制台。

当前发布版本：<code>v1.2.4-go</code> · [GitHub Releases](https://github.com/lichao199208/gptGrok2api/releases)

> 本项目通过逆向研究接入 ChatGPT 网页能力，不是 OpenAI 官方服务。上游协议、账号策略和可用模型可能变化。请使用你有权使用的账号，并遵守相关服务条款和当地法律。

## 能力

- OpenAI 兼容接口：Chat Completions、Responses、Anthropic Messages、图片和可编辑文件任务。
- OpenAI 图片：<code>gpt-image-2</code> 文生图、图生图和多参考图编辑。
- 多账号池：JWT、OAuth refresh token、账号分组、失败换号、限流冷却和并发调度。
- 代理出口：默认代理、代理池、代理组、订阅导入、节点健康检测和图片任务并发限制。
- 管理控制台：账号、代理、图片图库、日志、实时请求、提示词、备份和系统设置。
- 本地持久化：账号和配置使用 JSON 文件，队列可使用 JSON 或 Redis，图片保存在 <code>data/files/images/</code>。

## Docker 启动

要求 Docker Engine 24+、Docker Compose v2，以及能够访问 ChatGPT 的网络出口。

```bash
git clone https://github.com/lichao199208/gptGrok2api.git
cd gptGrok2api
cp .env.example .env
mkdir -p data logs
test -f data/auth_keys.json || printf '{"items":[]}\n' > data/auth_keys.json
docker compose -f docker-compose.go.yml up -d --build
curl -fsS http://127.0.0.1:3000/health
```

至少在 `.env` 中设置三个**互不相同**的随机密钥：

```dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-api-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
IMAGE_SCHEDULER_INTERNAL_KEY=replace-with-a-third-random-secret
CHATGPT2API_GO_PORT=3000
GO_PUBLIC_BASE_URL=http://your-server:3000
```

`IMAGE_SCHEDULER_INTERNAL_KEY` 保护 `/internal/image-scheduler`、`/internal/image-monitor`、
`/internal/logs/call` 三个端点，它们**对公网可达**。生成方式：`openssl rand -hex 32`。

> 该值在 `.env.example` 中刻意留空。未设置时这三个端点一律返回 503（fail-closed）——
> 这是安全的失败方向；而给一个示例默认值会让人直接沿用，等于把凭据公开在仓库里。

管理控制台地址为 <code>http://服务器地址:3000/</code>。图片队列网关默认只监听 <code>127.0.0.1:3001</code>，普通客户端直接访问主服务的 <code>/v1</code> 接口。

服务器部署可以叠加覆盖文件，将主服务绑定到 <code>127.0.0.1:8000</code>：

```bash
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
```

## API

请求使用 Bearer 认证：

```http
Authorization: Bearer <api-key>
```

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/health` | 健康检查 |
| `GET` | `/v1/models` | 当前模型目录 |
| `POST` | `/v1/chat/completions` | 文本聊天和图片兼容调用 |
| `POST` | `/v1/responses` | Responses 兼容接口 |
| `POST` | `/v1/messages` | Anthropic Messages 兼容接口 |
| `POST` | `/v1/images/generations` | 文生图 |
| `POST` | `/v1/images/edits` | 图生图和图片编辑 |
| `GET` | `/v1/files/image?id=...` | 下载生成图片 |
| `POST` | `/v1/editable-file-tasks` | 创建 PPT/PSD 文件任务 |
| `GET` | `/files/{path}` | 下载可编辑文件产物 |

文本示例：

```bash
curl http://127.0.0.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer your-api-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"介绍一下这个项目"}]}'
```

图片示例：

```bash
curl http://127.0.0.1:3000/v1/images/generations \
  -H 'Authorization: Bearer your-api-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"一只漂浮在太空里的猫","size":"1024x1024","n":1}'
```

图片编辑使用 `multipart/form-data`：

```bash
curl http://127.0.0.1:3000/v1/images/edits \
  -H 'Authorization: Bearer your-api-key' \
  -F 'model=gpt-image-2' \
  -F 'prompt=保留主体，把背景改成蓝色' \
  -F 'image=@reference.png;type=image/png'
```

公网部署时设置 `GO_PUBLIC_BASE_URL` 为外部 HTTPS 地址。图片文件默认保留 1 天，可用 `GO_IMAGE_RETENTION_DAYS` 和 `GO_IMAGE_CLEANUP_INTERVAL_SECONDS` 调整。

## 主要配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CHATGPT2API_AUTH_KEY` | 空 | 普通 API 密钥 |
| `CHATGPT2API_ADMIN_KEY` | 空 | 管理密钥 |
| `CHATGPT2API_GO_PORT` | `3000` | 本地 Compose 宿主机端口 |
| `GO_PUBLIC_BASE_URL` | 空 | 图片和文件公开 URL 前缀 |
| `GO_ROOT_DIR` | 当前目录 | 应用根目录 |
| `GO_DATA_DIR` | `data` | 运行数据目录 |
| `GO_CONFIG_PATH` | `config.json` | JSON 配置文件路径 |
| `GO_ACCOUNTS_PATH` | `data/accounts.json` | OpenAI 账号文件 |
| `GO_AUTH_KEYS_PATH` | `data/auth_keys.json` | 用户密钥文件 |
| `GO_QUEUE_BACKEND` | `json` | `json` 或 `redis` |
| `GO_REDIS_ADDR` | `127.0.0.1:6379` | Redis 地址 |
| `GO_REQUEST_TIMEOUT_SECONDS` | `180` | 上游请求超时 |
| `GO_CHAT_MAX_RETRIES` | `2` | 聊天和图片最大重试次数 |
| `GO_IMAGE_ACCOUNT_CONCURRENCY` | `1` | 单账号图片并发 |
| `GO_IMAGE_MAX_CONCURRENCY` | `128` | 单进程图片总并发 |
| `GO_IMAGE_RETENTION_DAYS` | `1` | 本地图片保留天数 |
| `GO_PROXY_URL` | 空 | 默认代理 |
| `GO_PROXY_POOL` | 空 | 逗号分隔的代理池 |

完整配置见 [`.env.example`](./.env.example) 与 [`config.example.yaml`](./config.example.yaml)。真实配置和凭据不要提交到 Git。

## 开发验证

```bash
go test ./...
go build ./...
cd web-vue
npm ci
npm run build
```

本项目参考并继承了 [yukkcat/chatgpt2api](https://github.com/yukkcat/chatgpt2api) 的接口和业务思路。分发时请保留仓库中的 [`LICENSE`](./LICENSE) 和历史来源归属文件 [`GROK2API_LICENSE`](./GROK2API_LICENSE)。
