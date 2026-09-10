# tasks/
> L2 | 父级: ../CLAUDE.md

任务队列：同一套 QueueAPI 接口下的两套实现，由 GO_QUEUE_BACKEND 选择。

成员清单
queue.go: JSON 文件队列——worker 轮询、Submit/Get/Cancel/List、原子落盘、损坏文件留档
redis.go: Redis 队列——RESP 手写协议、BLPOP 消费、索引集合、命令级超时

关键约定:
- 任何离开锁的 `*Task` 都必须是 `clone` 的快照，活指针不得交给 handler 或序列化。
  `clone` 必须是**递归**深拷贝：只重建顶层 map 的话，嵌套的 map/slice 仍与队列内的
  原件共享，handler 改一个嵌套值就改到了下一次 Submit 的输入。
- 命令必须带超时（调用方统一传 context.Background()，超时由 command() 兜底）

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
