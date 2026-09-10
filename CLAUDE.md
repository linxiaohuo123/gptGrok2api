# gptGrok2api（Go 版） - 把 ChatGPT 网页账号池包装成 OpenAI 兼容 API 的高性能网关
Go 1.23 + net/http + tls-client(浏览器指纹) + Redis/JSON 双队列 + Vue 3 + TypeScript + Vite + Tailwind

<directory>
cmd/ - 进程入口 (1子目录: gptgrok2api)
internal/ - 后端全部逻辑，12 个包，无 Web 框架（标准库 net/http） (12子目录: httpapi provider proxy store tasks protocol config model accounts auth oauth agentidentity)
web-vue/ - 控制台前端 (src 为源码，dist 为构建产物)
go-image-gateway/ - 独立图片排队网关，**独立 go module**，与主程序只通过 HTTP + Redis 交互
deploy/ - 部署编排 (4子目录: icloud-privacy-mail systemd launchd)
scripts/ - 运维、探测、排障脚本
utils/ - 前端注入用的 JS 资源
services/ - 默认提示词库
docs/ - 架构图

架构收敛：已物理清除 register 与 backups 空残留目录，专注 ChatGPT 核心网关与图片削峰。
</directory>

<config>
Dockerfile - 生产镜像（CI 用它，构建前端 + Go，最终阶段名 app）
Dockerfile.golang - 同上的变体，已同步创建 /app/logs 保持环境一致
docker-compose.yml - 主部署编排（含 iCloud sidecar profile）
docker-compose.go.yml - Go 栈编排（含 image-gateway 与独立 Redis）
docker-compose.warp.yml - 带 WARP/Privoxy/FlareSolverr 的出网编排
config.example.yaml - 运行期配置样例，**其中若干段 Go 端从未读取**，勿当契约
.env.example - 环境变量样例
VERSION - 版本号，与 internal/config/config.go 里的默认值需手工同步
</config>

## 构建与验证

```
export GOPROXY=https://goproxy.cn,direct   # 本机直连 proxy.golang.org 会超时
go build ./... && go vet ./... && go test -race ./...
```

三个独立 module，改哪儿测哪儿：根模块、`go-image-gateway/`、`deploy/icloud-privacy-mail/source/`。
`internal/*/regression_*_test.go` 是回归套件，**必须保持全绿**。

## 三条铁律（来自一轮全量审计，违反即事故）

1. **桩函数不许返回 200。** 未实现的接口要么不注册路由，要么明确报错。返回 200 的桩会用"假成功"掩盖契约错配——本仓库历史上多个前端按钮"永远报错"、WebDAV 面板"测试通过"，根因都是这个。
2. **快照 = 深拷贝；锁的边界必须包住最后一次数据访问。** `copy := *item` 只复制结构体，`Metrics`/`Events` 这类 map 仍与在途记录共享内存；出锁后再 `json.Marshal` 等于没加锁，Go 运行时抛的是**不可 recover 的 fatal error**。
3. **内部端点一律 fail-closed。** `/internal/*` 对公网可达，密钥未配置时必须拒绝服务，而不是放行。
4. **故障域严格隔离。** 客户端参数非法或提示词触犯审核拦截（HTTP 400）属于请求域，严禁跨账号轮换重试（防雪崩），严禁对账号累计失败与打入 Cooldown。

## 架构变更时

- 增删/移动文件 → 更新所在目录的 `CLAUDE.md`（L2）
- 接口签名、依赖、职责变化 → 更新该文件头部契约（L3）
- 顶级模块增删、技术栈变化 → 更新本文件（L1）

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
