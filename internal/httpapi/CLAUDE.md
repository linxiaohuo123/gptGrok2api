# httpapi/
> L2 | 父级: ../CLAUDE.md

HTTP 层全部实现：路由表、中间件、协议端点、管理端 API、运行时状态。**唯一组装层**——其它包只被这里依赖，不反向依赖。

成员清单
server.go: Server 结构、路由注册、withMiddleware、监控中间件、请求体形状解析
media.go: 图片生成/编辑公开入口（/v1/images/*），最长链路：排队→取号→出网→轮询→下载→落盘
openai_chat.go: /v1/chat/completions 的完整与流式实现
messages.go: Anthropic /v1/messages 与消息流
responses.go: OpenAI /v1/responses、responseFromChat、writeEventSSE（SSE 统一出口）
migration_routes.go: 迁移兼容路由——/internal/* 调度器与监控、备份/存储测试、iCloud 代理
admin_runtime.go: 运行时监控记录与快照、代理测试、调用日志落盘、redactProxyError
admin_extra.go: 管理端杂项——日志查询筛选、图片任务、提示词、模型目录
admin_web_contract.go: 管理端契约面——/api/settings 视图与嵌套合并、账号单资源端点（含 runAccountTest 真实账号测试）
chat_dedupe.go: 对话在途去重与短缓存，拦截并发连击与重试风暴
admin_media.go: 图片管理——列表分页、标签、删除、打包下载、公开图片服务
admin_backup.go: 备份——列表、运行（本地 zip）、删除、下载
account_refresh.go: 账号 AT 批量刷新与进度查询（容量上限 50 条防内存泄漏）
account_oauth_export.go: OAuth 账号导出（含 Agent Identity 归档）
proxy_subscription.go: 代理订阅拉取与节点替换
proxy_group_health.go: 代理组节点健康探测（并发 + 结果持久化）
proxy_runtime_failure.go: 运行时失败回写代理组与节点驱逐
management.go: 通用任务 API 与代理配置（profiles / groups）的增删改
external.go: 第三方账号源（CPA / Sub2API）存储与导入 job 状态机
external_api.go: 第三方账号源的 HTTP 端点与导入触发
survival.go: OpenAI 账号存活巡查与配置
image_retention.go: 图片过期清理调度
media_metadata.go: 图片元数据与标签落盘
disk_usage_unix.go: 磁盘容量（Unix）
disk_usage_windows.go: 磁盘容量（Windows）
hourly_metrics.go: 增量小时指标聚合器，写时折叠 O(1) 秒级仪表盘查询
diagnostics.go: 全局统一脱敏清洗器，截断代理账密与 Token 凭据
content_filter.go: 敏感词检测拦截与全局系统提示词注入

关键约定:
- `/internal/*` 一律 fail-closed：密钥未配置时返回 503，不放行
- **投影必须在锁内完成，且不得把活容器交出去。** 三条同时成立才算数：
  ① 不在锁外读锁内取出的对象（`RLock(); t := m[k]; RUnlock(); 读 t` 是错的）；
  ② 不用 `copy := *t` 冒充快照——它只复制结构体，切片/map 字段仍与原件共享；
  ③ 投影结果里不能含活引用（`value["result"] = task.Result` 即使锁内赋值，
  `writeJSON` 也是在解锁之后才序列化，等于没加锁）。正确范式见
  `account_refresh.go` 的 `refreshProgressAPI`（锁内 `cloneMap` 后再出锁）
- **响应形状必须以 `web-vue/src/api/*.ts` 的前端契约为准，且投影集中在一处**：
  图库走 `galleryRowForAPI`（admin_media.go），图片任务走 `imageTaskPublic`
  （admin_extra.go），账号写操作走 `mergeAccountMutation`（admin_web_contract.go），
  监控走 `monitorRecordPresentation`（admin_runtime.go），调用日志走
  `formatCallSummary`（admin_extra.go）的 `presentation`。
  前端的解析器是强校验——字段缺失或类型不符会直接抛错并让整页不可用，而不是静默降级。
  账号写操作的抛出点尤其危险：它在返回对象字面量里求值，此时后端**已经写库成功**，
  于是"编辑保存失败"其实已保存、"导入失败"其实已入库。改动这些投影时同步更新
  `regression_*_contract_test.go` 里的对应守卫
- **客户端可控的字符串不得直接作为聚合 map 的键**。model / endpoint / error_code
  都来自请求体，必须过 `metricKey`（hourly_metrics.go）收敛长度与基数；超限归入
  `__other__` 而非静默丢弃——丢弃会让"分项之和 ≠ 总数"变成更难查的坑。
- **裁剪 / 过期一律以真实当前时间为基准**，不得用事件时间。一条未来时间戳的记录
  会把 cutoff 推到未来，一次抹掉全部历史（`pruneOldBucketsLocked` 踩过）。
- **切片字段一律 `make` 出非 nil 再返回**。`append([]T(nil), 空...)` 返回 nil，
  序列化成 JSON `null`；而前端常写成 `data.value?.slow.slice(...)`——`?.` 只护住了
  `data.value`，没护住 `.slow`，`null.slice` 直接抛 TypeError 让整个 computed 失败。
- **请求体的账号选择必须同时认 `account_ids` / `selection` 与 token 字段**。
  前端只发前两种形态，而 Go 侧历来只读 `access_tokens` / `tokens`；读不到时的
  后果分两类——直接 400，或更糟的静默 no-op（返回 200 却一个账号都没改）。
  统一走 `accountSelectionBody.refs()`，并且**空目标必须明确报错**

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
