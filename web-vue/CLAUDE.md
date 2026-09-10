# web-vue/
> L2 | 父级: ../CLAUDE.md

控制台前端：Vue 3 + TypeScript + Vite + Tailwind，构建产物 `dist/` 由后端作为静态站点托管。

成员清单
src/: 全部源码（详见 src/CLAUDE.md）
public/: 静态资源（logo 等）
dist/: 构建产物，**不要手工编辑**
vite.config.ts: 构建与开发代理配置（/api /v1 /auth 等转发到后端）
package.json: 依赖与脚本（`npm run build` 会先跑 tsc，类型错误直接中断构建）

注意: 仓库同时存在 package-lock.json 与 pnpm-lock.yaml，Dockerfile 走的是 `npm ci`。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
