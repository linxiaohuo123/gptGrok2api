# deploy/icloud-privacy-mail/
> L2 | 父级: ../CLAUDE.md

iCloud 隐私邮箱附属服务（**独立 go module**）。提供 Apple 账号登录、iCloud 邮箱创建与取码，由主程序按 `ICLOUD_PRIVACY_MAIL_BASE_URL` 调用。

成员清单
Dockerfile: 构建镜像；注意其中未声明 HEALTHCHECK，健康检查写在 compose 里（探测 `GET /login`）
source/cmd/panel/main.go: 服务入口
source/internal/app/: 全部实现——HTTP 服务与路由、Apple 登录（含 SRP 与 2FA）、IMAP 取码、iCloud 客户端、邮箱调度器、状态存储、自更新

对外: 仅在被 compose 网络内部访问，不发布端口。鉴权靠 `IPM_API_KEY`。
自更新: `/api/update/apply` 需管理员会话，下载后校验 sha256 再替换可执行文件；清单与产物同源，校验值只能防传输损坏，防不了源被篡改。
注意: `POST /api/auth/register` 对可达方开放（首个注册者即管理员），因此本服务**不可直接暴露到公网**。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
