# internal/
> L2 | 父级: /CLAUDE.md

12 个包，零 Web 框架，全部基于标准库 net/http。依赖方向单一：`httpapi` 是唯一的组装层，其余包互不反向依赖。

成员清单
accounts/: 账号池——租约分配、账号并发槽、失败计数与冷却
agentidentity/: Codex Agent Identity 加密归档，私钥不落普通账号列表
auth/: API Key / 管理员密钥校验与 Identity 解析，含 auth_keys.json 摘要匹配
config/: 配置装载——环境变量优先，config.json 兜底，含全部默认值与钳制
httpapi/: HTTP 层全部处理器、路由、中间件与运行时状态（唯一组装层，23 个文件）
model/: 模型目录与聊天路由解析（catalog.go 目录，chat.go 路由）
oauth/: OpenAI OAuth 登录流程与 token 存储
protocol/: 协议转换与工具调用（OpenAI/Anthropic/Responses ↔ 内部 Message）
provider/: 上游 ChatGPT 交互——图片生成、对话、可编辑文件、浏览器指纹、Sentinel
proxy/: 出网选择——上游路由表与图片代理组租约
store/: JSON 持久化——账号、密钥、配置，带内存缓存与合并落盘
tasks/: 任务队列抽象——JSON 文件与 Redis 两套实现

法则: 成员完整·一行一文件·父级链接·技术词前置

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
