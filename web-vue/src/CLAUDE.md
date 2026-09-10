# web-vue/src/
> L2 | 父级: ../CLAUDE.md

前端源码。分层约定：`views` 只做编排，`api` 独占后端契约，`lib` 放纯函数，`composables` 放可复用状态。

成员清单
main.ts: 应用入口
App.vue: 根组件
api/: 后端契约层——每个文件对应一族端点，**类型声明即接口契约**（20 个文件: accounts settings logs monitor proxy gallery imageTasks icloud …）
components/: 展示组件 (3子目录: ai studio ui)
composables/: 可复用状态逻辑（确认框、悬浮菜单、分页、模型目录、窗口化列表）
config/: 静态配置（模型目录兜底、项目元信息）
layouts/: AppShell.vue——全局外壳、导航、KeepAlive 缓存名单
lib/: 纯函数与适配器（主题、图表、下载、偏好、Markdown 渲染、代码格式化）
router/: 路由表与导航守卫（含登录态判定）
stores/: Pinia store（auth 鉴权、settings 设置）
types/: 后端数据结构类型（api.ts 是契约总表）
views/: 页面级组件 (9子目录: accounts dashboard gallery logs monitor proxy settings studio + 空残留 register)

关键约定:
- `api/client.ts` 的响应拦截器返回 `response.data`，因此 `apiClient.post<T, { result: X }>` 里的 `{result:X}` 就是**响应体的字面形状**；后端少包一层 `result` 就会在前端抛 TypeError
- `AppShell.vue` 的 `cachedRouteNames` 按组件 `__name` 匹配，改名会让 KeepAlive 静默失效

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
