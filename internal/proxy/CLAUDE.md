# proxy/
> L2 | 父级: ../CLAUDE.md

出网选择：决定每个请求从哪个出口出去，并把出口质量反馈回代理组。

成员清单
router.go: 上游路由表——upstreams.txt 解析与轮询选择
transport.go: 出网传输——自定义 http.Transport（HTTP/SOCKS4/SOCKS5）、图片代理组 lease、节点容量与驱逐、健康降级

关键约定:
- 图片 lease 的 Release 必须具备幂等性（sync.Once）
- 代理组可能被配置重载过滤成 0 节点，任何按节点数计算的容量都要先判空
- **节点健康状态按 URL 归一，存在 `Manager.imageHealth` 里，不得寄生在 `imageNode` 对象上。**
  `ConfigureImageGroups` 每次重载都会重建 `imageNode`，而在途 lease 握的是旧指针——
  计数若挂在节点对象上，那次成功/失败就写进了没人引用的对象：successes 停在 1、2 时
  既不触发持久化事件、又会被下次重载打回配置快照，节点永远攒不够 `imageNodeStableSuccess`，
  `pickStableNodeLocked` 永远选不中它；驱逐按指针比对也会静默失效。
  配置里的 `runtime_failure_count` 只是**首次见到该 URL 时的种子**，之后健康表是权威来源。
- 驱逐必须按 **URL** 从组里移除，不能按 `*imageNode` 指针比对

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
