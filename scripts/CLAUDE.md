# scripts/
> L2 | 父级: ../CLAUDE.md

运维、探测与排障脚本，均为独立可执行文件，不被程序引用。

成员清单
tscheck.sh: 前端类型检查
inspect_app_run.sh: 容器内运行时状态取样
inspect_container.sh: 容器与镜像检查
remote_blackbox.sh: 远端黑盒探测
install_cffi.sh: curl_cffi 安装辅助
privoxy-warp.conf: WARP 编排用的 Privoxy 配置
mailcom_domains.txt: 邮箱域名清单（注册用）
mailcom_domains_ts.txt: 域名清单（带时间戳变体）
mailcom_domains_clean.txt: 清洗后的域名清单
mailcom_mothers.txt: 母号清单

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
