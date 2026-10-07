# Phase 9B.5b.1：Provider 能力证据台账

本段完成能力与限制来源的安全记录基础，不代表 9B.5b 全部完成。协议能力探测和发件域 DNS 诊断继续在 9B.5b.2 实现；真实 SMTP 投递及目标邮箱身份验证仍需独立授权与验收。

## 记录与信任边界

使用已有 append-only `maintenance_audit` 存储 `PROVIDER_CAPABILITY_EVIDENCE`，不修改 schema，不写入 Provider 配置，不改变 worker 或 failover 的判断。证据永久标记 `trust=operator_claim`、`affects_delivery=false`。`source` 只能为 `operator_report` 或 `provider_documentation`；文档 URL 也是人工引用，不表示系统抓取、验证或 Provider 签名认证。

支持 `max_message_bytes`（1–1 GiB 的规范十进制数）、`max_recipients`（1–100,000），以及 `smtputf8`、`8bitmime`、`provider_dkim_signing`（supported / unsupported / unknown）。数量只描述声明的限制，不能替代当前网关大小、SMTP 实际协商和共享额度执行。无法在此格式表示的限制保持未记录，不允许用 0 冒充无限。DKIM 声明不证明签名有效、DMARC 对齐或入箱。

URL 限制为最多 2048 字节的 HTTPS 引用，拒绝嵌入账号、查询参数、fragment、非法 UTF-8 和控制换行；不进行网络访问。公开文档可通过 URL 引用；私有客服工单和带 token 的链接不要写入，使用人工报告。actor/reason 是审计文字，不能包含密码、密钥、邮件原文或收件地址。

观察时间必须明确提供，使用 RFC3339，不能超过服务时间 5 分钟，年份须可由数据库读取。服务器记录时间与观察时间独立。查询标出当前 Provider revision、`configuration_changed` 与 `older_than_30_days`；30 天只是提示复核的固定窗口，不是安全有效期或自动过期规则。凭证轮换也会改变 revision，旧证据保守提示复核。即使没有这些提示，人工记录仍不构成协议或投递证明。

## CLI

使用既有受限运行数据库账号；命令进入辅助进程占用保护，与在线服务共存并阻止离线迁移/恢复。通过现有 Provider 查询取得 ID 与 revision，再记录：

```sh
relaytale record-provider-evidence \
  --id <provider-uuid> --expected-revision <revision> \
  --topic max_message_bytes --value 25000000 \
  --source operator_report --observed-at <RFC3339时间> \
  --actor operator --reason provider-plan-review

relaytale list-provider-evidence --id <provider-uuid> --limit 100
```

写入在同步提交事务内锁定 Provider 并核对 revision，旧 revision 拒绝提交；审计写入失败不返回成功。操作不修改 revision，因此同一配置可记录多条不同来源的证据。冲突时重新读取配置，不盲目替换 expected revision；提交结果不确定时先查询历史再决定是否重录。

查询返回最新记录优先的数组，上限 1,000 条，默认 100；空数组只表示该查询未找到记录，不保证 Provider 存在。历史不会因新记录被覆盖；更多记录使用现有审计导出。不同时间/来源冲突的声明全部保留，需要人工选择依据，本阶段不自动选择“最新即正确”。目前为 CLI 功能，未接管理 API 或 Web UI。返回条数上限不等于数据库扫描成本上限；大规模审计历史的查询容量与专用索引仍需后续验收。

## 验收

测试覆盖不允许伪装协议探测来源、敏感 URL 拒绝、数量格式约束、观察时间约束；隔离数据库测试覆盖 revision 冲突、审计失败、旧证据可见、配置变化与 30 天复核提示，以及记录操作不会产生投递 attempt。完整 `make ci` 通过：格式检查、静态分析、构建和竞态测试，隔离数据库集成耗时 236.559 秒。最终日期校验补测后再次通过格式、静态检查、构建与定向竞态测试（1.967 秒）。实际二进制演练通过受限账号写入/查询、高权限账号拒绝，以及密钥轮换后旧记录复核提示；秘密扫描无泄漏。没有连接真实 Provider 或查询 DNS。
