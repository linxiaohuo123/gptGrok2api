# model/
> L2 | 父级: ../CLAUDE.md

模型与路由：对外暴露的模型清单，以及"某个模型该走哪条上游通道"的判定。

成员清单
catalog.go: 模型目录——对外模型列表（含 gpt-image-2.5 家族与 Codex 系列）、IsImageModel 图片模型判定、模型规格
chat.go: 聊天路由——ResolveChat 判定走 OpenAI 池还是本地，含全系图像模型的 chat 兼容通道

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
