# go-image-gateway/
> L2 | 父级: ../CLAUDE.md

**独立 go module**。图片排队网关：把并发的图片请求压成一个 Redis 队列，由固定数量的 worker 串行取号执行，对上游做削峰。

成员清单
main.go: 主流程——配置装载、鉴权（auth_keys.json 摘要）、入队与等待、worker 消费、调度器 reserve/execute/release、监控与调用日志上报
client.go: 出网客户端构造与 SSRF 防护——newBackendClient、newRemoteFetchClient、isBlockedRemoteAddress、fetchRemoteImage

对外接口: `/v1/images/generations`、`/v1/images/edits`、`/v1/image-tasks/{id}`、`/health`

关键约定:
- **两类出网 client 必须分开**。`client` 调调度器/主程序，刻意不设 `Client.Timeout`（生图可跑满
  BackendTimeout），只靠 DialContext/TLSHandshakeTimeout 保证连接建立不挂死；`fetchClient` 抓用户提供的
  image_url，短超时 + `Dialer.Control` 私网拦截（含 DNS rebinding），绝不能互换。
- **准入判据是「积压 + 在飞」**（`LLen + active`）。只看 `LLen` 是失效的：worker 空闲即 BLPOP 取走任务，
  满载时队列长度恒为 0，`QueueCapacity` 永远触发不了。
- 租约释放等收尾动作一律用有界 ctx，不得用 `context.Background()`。

已知缺口:
- 每次生图向调度器 reserve 时模型硬编码为 `gpt-image-2`，不随请求变化
- 队列用 BLPOP 取出后无确认机制，worker 崩溃即丢任务
- (已修复) 主程序 Go 版调度器的 execute 契约已接入真实生成逻辑，网关与主程序闭环
- (已修复) image_url 抓取的 SSRF 与超时缺口：改为受限 client，见 `client.go`

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
