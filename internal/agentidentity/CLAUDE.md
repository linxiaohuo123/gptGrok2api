# agentidentity/
> L2 | 父级: ../CLAUDE.md

Codex Agent Identity 的独立加密归档：私钥单独落盘，不写进普通账号列表。

成员清单
store.go: 加密存档——AES 加密的 agent 身份读写、Ensure 注册、Summary/AuthJSON 导出

关键约定:
- **`Ensure` 的网络段必须在锁外。** `s.mu` 是全局锁，`Summary`/`AuthJSON` 都被它挡住；
  而全量导出是 N 个账号串行调用本函数——持锁做 I/O 意味着管理面冻结 N×30 秒。
  结构固定为三段：锁内查缓存 → 锁外生成密钥并注册 → **锁内重新读盘**再追加。
- 第三步的重新读盘不能省：`save` 是全量替换，用注册前的旧快照回填会把并发写入的
  其它账号身份一起覆盖掉。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
