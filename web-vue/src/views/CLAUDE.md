# web-vue/src/views/
> L2 | 父级: ../CLAUDE.md

页面级组件。约定：页面只做编排，重逻辑下沉到同目录的 `*.ts` 或 composables。

成员清单
Accounts.vue: 账号管理页（最大的页面，逻辑在 accounts/ 目录）
accounts/: 账号页逻辑——列表、批量操作、导入、分组
Dashboard.vue: 概览
dashboard/: 概览页数据装配
Studio.vue: 对话/生图工作台
studio/: 工作台逻辑——会话状态、图片任务轮询、代码块
Gallery.vue: 图片库
gallery/: 图库逻辑——查询、交互、文件操作
Logs.vue: 调用日志
logs/: 日志逻辑——筛选、详情时间线
Monitor.vue: 实时监控
monitor/: 监控数据装配
Proxy.vue: 代理管理
proxy/: 代理组运行时
Settings.vue: 设置
settings/: 设置各面板与运行时逻辑（备份、代理、用户密钥、图片存储、错误话术）
Login.vue: 登录
Docs.vue: 文档页
DebugCenter.vue: 调试中心

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
