# store/
> L2 | 父级: ../CLAUDE.md

持久化层：账号、密钥、配置三份 JSON 文件的唯一读写入口，带内存缓存与写合并。

成员清单
json.go: Store——账号缓存（copy-on-write + revision）、密钥缓存（2 秒 TTL）、配置读写、原子写（temp+fsync+rename）、延迟合并落盘
clone.go: CloneMap——JSON 形状数据的深拷贝，本包与 httpapi 共用的唯一实现

关键约定:
- 请求主路径上的读操作**不得写盘**：`Authenticate` 只改内存里的 last_used_at，由定时器合并落盘
- 所有对外返回的账号/密钥/配置必须是深拷贝，缓存容器不得泄漏给调用方。
  复制语义只有 `CloneMap` 一处实现（历史上 store 与 httpapi 各有一份逐字相同的
  浅拷贝，修一份必漏另一份；浅拷贝让 `configCache` 的嵌套 map 被调用方在锁外改写）

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
