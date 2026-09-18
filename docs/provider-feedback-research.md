# SpaceMail / Purelymail 反馈能力调查

调查日期：2026-09-17。只读取公开官方资料，未登录邮箱、调用管理 API、变更账号或发送测试邮件。以下区分文档证据、推断与待实测项，不能将“未找到公开接口”写成“服务绝不支持”。

## Purelymail

1. **公开 API 存在，但当前读到的规范不是投递事件 API。** 官方新闻域上的 [Swagger 页面](https://news.purelymail.com/api/index.html) 引用 [swagger-spec.js](https://news.purelymail.com/api/swagger-spec.js)。本次直接读取该文件，版本 0.0.1，共 19 个操作，属于用户、密码恢复、路由、域名、应用密码和余额管理，使用 Purelymail-Api-Token；没有 bounce、delivery event、消息查询或签名 webhook 操作。规范发布在新闻域，不能保证覆盖现有全部后台能力。主站同路径本次返回 404，不能把搜索引擎旧摘要当作当前主站规范。
2. **IMAP 收集可行。** [技术配置](https://smtp.purelymail.com/docs/setup/technical) 明确提供 imap.purelymail.com:993，使用 TLS；启用 2FA 时使用应用密码。它证明可以认证读取邮箱，不证明每封来信的作者或内容真实。
3. **专用退信地址有可研究的路由基础。** [Routing 文档](https://smtp.purelymail.com/docs/routing) 支持前缀、精确地址和 catch-all；[Symbolic Subaddressing](https://support.purelymail.com/support/solutions/articles/159000406702-symbolic-subaddressing) 描述子地址规则，并说明其早于账号路由和 Sieve 执行。因此不能假设 bounce+TOKEN 经处理后一定仍能从可信信封元数据恢复令牌。文档对点号的概述与技术说明也不完全一致，实际行为需要样本确认。
4. **MAIL FROM 必须符合发送权限。** [发送权限说明](https://support.purelymail.com/support/solutions/articles/159000433207-error-530-5-7-1-you-x-are-not-authorized-to-send-mail-as-y-) 明确同时验证信封发件人和 Header From。默认与受限 Send As 策略不同；这不等于任意 VERP 地址已经获得当前账号许可，也没有证明 Return-Path 不被改写。
5. **5.1.1 不是永久无效地址的充分证据。** [官方排障说明](https://support.purelymail.com/support/solutions/articles/159000413730-error-user-isn-t-located-here-error-5-1-1-) 列出的原因包括用户不存在、缺少路由、路由目标错误和规则格式错误。未来自动抑制必须考虑错误归属与配置问题；7B.1 的 ADDRESS_INVALID_REPORTED 仅是待查分类，不能升级为确定事实。
6. [2026-09-14 更新](https://news.purelymail.com/posts/updates/2026-09-14-platform-updates.html) 涵盖 From 校验和出站过滤，没有宣布投递反馈 webhook。更新内容不能证明不存在其他未公开能力。

## SpaceMail / Spaceship

1. [公开 Spaceship API 文档](https://docs.spaceship.dev/) 包含域名、DNS、联系人、异步任务和其他平台管理能力；本次读取的页面没有 Spacemail、bounce 或 webhook 条目。平台 API 凭证不等于已开放邮件投递事件 API。
2. [官方客户端配置](https://www.spaceship.com/knowledgebase/connect-spacemail-to-email-client/) 明确支持 mail.spacemail.com:993 的 TLS IMAP。可设计专用邮箱收集器，具体账号是否启用访问仍需核实。
3. [Catch-all 文档](https://www.spaceship.com/en-GB/knowledgebase/setup-catch-all-mailbox-spacemail/) 明确可将域内不存在地址的邮件汇集到指定邮箱。它只能证明入站路由能力，不证明允许用这些地址作为 MAIL FROM，也不保证导出后原始信封收件地址仍可验证。
4. 当前公开资料不足以确认签名退信 webhook、逐封投递事件 API、DSN 扩展 ENVID/ORCPT 支持或信封保持行为。这些仍是实测/客服问题，未擅自询问客服。

## 对项目的修正建议

不必把“找到签名 webhook”作为继续开发的唯一条件。可以先实现 **IMAP → 7A 证据存档 → 待查队列**，把自动采集与自动处置分开验收：

- 仅抓取指定专用邮箱/文件夹；TLS 验证、独立凭证、只读访问，不删除、移动或标记已读。
- 以稳定账号标识、文件夹、UIDVALIDITY 与 UID 建立外部事件 ID；数据库提交成功后才推进游标。连接重试不重复处理，UIDVALIDITY 变化不能静默沿用旧游标。
- 邮件大小、批量、时间和存储容量均有上限；超限记录为显式待处理问题，不能截断后当完整证据，也不能让一封大信永久堵住后续采集。
- 不因 From、Authentication-Results 字符串、DKIM 字段或匹配令牌就自动授予信任。令牌也可能被收件人看到，它提供关联线索而不是不可伪造的退信来源。
- 后续独立验证：授权的 MAIL FROM、返回后的实际 Return-Path、令牌经过路由后的保存位置、真实 DSN 的机器字段和来源证据、同一邮件的多收件人反馈。没有可信来源时继续人工处置。

调查后的实现进度：已增加 [Phase 7B.2 只读 IMAP 收集器](phase-7b2-imap-collection.md)，显式启用后把原件存入 UNVERIFIED 待查队列。真实 Provider IMAP 尚未验收，透明模式保持不变；自动抑制继续作为后续独立验收项。
