# Phase 9B.5b.2：TLS 后能力观察与精确域名 DNS 诊断

本段接入两项显式诊断，不改变默认投递、重试、failover 或身份授权。协议观察与 9B.5b.1 人工证据分开存放，不能通过人工命令冒充探测。

## Provider 协议观察

已有 `test-provider --id <uuid>` 仍只连接、协商 TLS、认证、QUIT，不发送 MAIL/RCPT/DATA。输出及 `PROVIDER_DIAGNOSTIC` 审计增加加载配置时的 `provider_revision` 和 `capabilities`。配置可能在探测过程中改变，应与当前 revision 对照；报告不代表新配置已验收。

`capabilities=null` 表示未完成安全观察，不表示所有能力都不支持。能力只采自 TLS 建立后的 EHLO；STARTTLS 之前的信息不沿用，符合 [RFC 3207](https://www.rfc-editor.org/rfc/rfc3207) 的协商重置要求。首行服务器标识不当作扩展。只保留 SMTPUTF8、8BITMIME、PIPELINING、AUTH PLAIN/LOGIN 的 advertised 布尔值和 SIZE 状态，不将其它扩展文字写入新增能力字段。

SIZE 状态为 absent / unspecified / numeric / invalid；numeric 的字节数是声明值，0 不能作为网关无限制许可。重复、负值、溢出及非数值声明标为 invalid。能力观察没有改变现有 client 的 AUTH 选择或发送逻辑；PIPELINING 被广告也不会自动启用流水线。

TLS 成功但认证被拒时仍可能观察到能力，不能将能力存在当作认证成功。READY 只表示本次握手与认证成功，不能证明账号允许发件域、远端接受邮件、签名对齐或实际入箱。审计标记 `trust=tls_ehlo_observation`、`affects_delivery=false`，本段不修改 provider_health，不自动切换或解除熔断。

## 发件域 DNS 诊断

```sh
relaytale diagnose-sender-domain --domain example.test --selector s1
```

使用受限运行数据库账号及辅助进程占用保护。命令明确发起 DNS 查询，域名使用 ASCII/punycode 形式，不接受 IP、地址字符串、空标签或非法标签；selector 可省略。总查询期限为 5 秒，最多查询域名本身、`_dmarc.<domain>`、指定 `<selector>._domainkey.<domain>` 三个 TXT 名称。输出与 `SENDER_DOMAIN_DIAGNOSTIC` 审计只记录状态和候选数量，不保存 TXT 原文、DKIM 公钥或报告邮箱。

状态为 candidate_present / no_candidate / not_found / lookup_failed，省略 selector 时 DKIM 为 not_checked。匹配只是版本前缀或 p 标签候选，不是完整语法与有效性验证；多个 SPF 候选、空或撤销的 DKIM p 标签都不能被当作正常认证。lookup_failed 与 not_found 分开，失败不会冒充不存在。

该命令不展开 SPF include，不代入实际发送 IP 或 MAIL FROM，不计算组织域 DMARC fallback，不验证 DNSSEC、DKIM 签名或签名身份对齐。精确子域没有 DMARC 候选，不代表不存在适用的上级策略；DMARC 策略发现与身份对齐边界见 [RFC 7489](https://www.rfc-editor.org/rfc/rfc7489)。查询 DNS 与 Provider 的发件域授权是独立问题：DNS 记录存在不能证明账号授权该 From 域，也不能许可跨 Provider 切换。

`affects_delivery=false` 始终成立。异常或候选存在都由运维复核，不能隐式修改 from_domains、共享额度或 UNKNOWN 状态。诊断不收集实际邮件或收件人，不需要发测试邮件。

## 验收与后续

隔离 Fake SMTP 测试覆盖 STARTTLS、implicit TLS、缺少 STARTTLS、AUTH 拒绝，检查只采 TLS 后能力且不发送邮件数据；解析测试覆盖服务器首行、重复/溢出/非法 SIZE。注入的 DNS resolver 验证精确查询名称、无 include/组织域探测、多个候选及无效域名拒绝。完整 `make ci` 通过：格式、静态分析、全项目构建及竞态测试，隔离数据库集成耗时 238.416 秒。秘密扫描未发现泄漏。测试未连接真实 Provider 或查询真实域名。

本段未接管理 API/UI，没有真实 Provider 与 DNS 链路验收。9B.5b 已有人工证据、协议观察与精确 DNS 诊断基础；组织域策略发现、自动认证评估、同一发件域跨 Provider 身份验收及大规模诊断容量仍需后续设计，不能因此宣布身份或生产链路已通过。
