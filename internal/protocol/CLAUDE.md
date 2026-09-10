# protocol/
> L2 | 父级: ../CLAUDE.md

协议层：把三套对外协议（OpenAI Chat Completions / Anthropic Messages / OpenAI Responses）翻译成内部 Message。

成员清单
chat.go: 协议转换——四种输入形态归一，contentText 只认"带字符串 text 字段"的内容块（text / input_text / output_text 通用）
tools.go: 工具调用——提示词注入与 tool_calls 解析

注意: `tools.go` 当前**零调用点**，客户端传 tools 会被静默降级成纯文本。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
