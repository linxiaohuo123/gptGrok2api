# provider/
> L2 | 父级: ../CLAUDE.md

上游交互层：直接与 chatgpt.com 对话，处理账号凭据、浏览器指纹、Sentinel 挑战与图片全流程。

成员清单
openai_image.go: 图片生成/下载主链路——bootstrap、requirements、会话准备、轮询、结果解析、落盘；doRequest 是出网统一入口
openai_chat.go: 对话——SSE 流式解析、事件增量、上游错误语义还原（detail → 401/429/400）
openai_account.go: 账号级能力——单账号指纹构造与隔离、clearance 缓存与刷新、/me 与账户检查
openai_browser.go: 浏览器指纹传输（tls-client / fhttp，对齐 Chrome 133），无状态传输杜绝跨账号 Cookie 串号
openai_editable.go: 可编辑文件任务（PPT/PSD 等）的上传与导出
sentinel_turnstile.go: Sentinel / Turnstile 挑战求解

关键约定:
- `doRequest` 的两条传输路径（标准库 / 浏览器）共用一次非 2xx 校验，任何新增传输都必须走同一出口
- 图片下载必须同时校验状态码与读取上限，否则错误页会被当成图片入库
- 指纹的设备/会话 ID 依账号确定性隔离并保持请求间稳定，不得全池共用同一 ID，亦不得每请求随机生成

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
