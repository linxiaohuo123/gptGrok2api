# web-vue/src/api/
> L2 | 父级: ../CLAUDE.md

后端契约层：每个文件对应一族端点，导出的 TS 类型就是前端对后端的**全部假设**。

成员清单
client.ts: axios 实例与拦截器——鉴权头注入、401 跳登录、错误归一化；**响应拦截器返回 response.data**
index.ts: 统一出口，聚合导出各 api 模块
accounts.ts: 账号列表、增删改、批量操作、刷新进度
accountImports.ts: 第三方账号源导入任务轮询
reverseAccounts.ts: 反向账号（外部投递）相关端点
auth.ts: 登录/登出/鉴权状态
settings.ts: 设置读写、备份、图片存储、保留期清理
logs.ts: 日志查询与详情
monitor.ts: 实时监控
stats.ts: 概览统计
gallery.ts: 图片库列表、标签、批量操作
imageTasks.ts: 图片任务状态与配额
chatStream.ts: 对话流式读取
proxy.ts: 代理配置、测试、订阅、健康
models.ts: 模型目录
prompts.ts: 提示词库
userKeys.ts: 用户密钥管理
debug.ts: 调试中心
version.ts: 版本与更新信息
icloud.ts: iCloud 邮箱桥（**多数端点后端未实现**）

关键约定: 声明 `{ result: X }` 的端点，后端必须真的包一层 `result`。历史上 image-storage / backup 三个端点漏包，导致对应按钮 100% 抛 TypeError。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
