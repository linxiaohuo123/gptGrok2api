# config/
> L2 | 父级: ../CLAUDE.md

配置装载：环境变量优先，config.json 兜底，所有数值型配置在此统一钳制上下界。

成员清单
config.go: Load——路径解析（GO_ROOT_DIR/GO_DATA_DIR）、全部默认值、数值钳制、旧版 config.json 迁移

注意: `Version` 默认值硬编码在 config.go，与仓库根的 VERSION 文件需手工同步。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
