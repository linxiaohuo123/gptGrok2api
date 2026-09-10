# deploy/
> L2 | 父级: ../CLAUDE.md

部署编排与附属服务。

成员清单
install.sh: 一键安装脚本——交互式选择存储后端与编排，下载 compose 与配置，起容器
docker-compose.server.yml: 服务器编排（端口绑定 127.0.0.1，交由 nginx 前置）
gpt.muyuai.top.nginx.conf: nginx 站点（location / 全量转发，**不区分 /internal**）
pro.muyuai.top.nginx.conf: 同上，另一域名
systemd/: chatgpt2api.service.example——裸机 systemd 单元样例
launchd/: com.chatgpt2api.app.plist.example——macOS launchd 样例
icloud-privacy-mail/: iCloud 隐私邮箱附属服务（**独立 go module**，作为 sidecar 运行）

已知缺口（尚未修）:
- install.sh 在 `--with-warp` 分支下载 `scripts/init_proxy_config.py`，该文件不存在，`set -e` 下必然中断
- install.sh 的 `MODE=python` 分支执行 `uv run uvicorn main:app`，本仓库没有 Python 入口，属死代码

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
