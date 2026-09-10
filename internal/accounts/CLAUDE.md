# accounts/
> L2 | 父级: ../CLAUDE.md

账号池：把账号文件变成可并发租用的资源，负责并发上限、失败计数与冷却。

成员清单
pool.go: 账号池——Lease 租约（sync.Once 保证只释放一次）、账号与图片两套并发槽、failures/cooldowns 表、Feedback 区分客户端中断(499)与故障、wake 唤醒广播

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
