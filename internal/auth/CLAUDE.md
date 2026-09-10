# auth/
> L2 | 父级: ../CLAUDE.md

鉴权判定层：把请求头里的令牌解析成 Identity，是 httpapi 唯一信任的鉴权入口。

成员清单
auth.go: Validator——API Key / 管理员密钥校验、Identity 解析、auth_keys.json 摘要回退匹配

注意: `AdminKey` 接受 `?app_key=` 查询参数，密钥会进入访问日志与浏览器历史。

[PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
