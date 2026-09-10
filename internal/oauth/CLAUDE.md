# oauth/
> L2 | 父级: ../CLAUDE.md

OpenAI OAuth：登录流程与凭据存储。

成员清单
openai_login.go: OAuth 登录——授权链接构造、code 换 token、state 校验
store.go: token 存储——加密落盘与读取

注意: `store.go` 当前无调用点。其 AES 密钥取自 `sha256(secret)`，接线前必须先做空 secret 校验，否则等于用公开常量加密全部 token。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
