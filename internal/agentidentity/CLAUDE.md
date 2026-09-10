# agentidentity/
> L2 | 父级: ../CLAUDE.md

Codex Agent Identity 的独立加密归档：私钥单独落盘，不写进普通账号列表。

成员清单
store.go: 加密存档——AES 加密的 agent 身份读写、Ensure 注册、Summary/AuthJSON 导出

注意: `Ensure` 在持有全局互斥锁期间发起网络请求，锁会一直握到请求返回。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
