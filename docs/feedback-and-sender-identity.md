# 可信反馈与发件身份责任

本文件区分已经实现的边界和后续接入设计。Phase 5A 只提供人工抑制与强制执行；尚无自动退信消费者、webhook 接口、VERP、SMTP DSN 扩展或反馈隔离队列。

## 当前责任

RelayTale 保存原始 EML 并透明转发，不自行生成 DKIM 签名、不插入缺失 Message-ID，也不改写信封发件人为退信专用地址。Provider 可按其配置签名或处理信封；运行方需核实实际 Return-Path 与退信邮箱归属。当前 `from_domains` 是路由许可列表，不证明域名 DNS、DKIM 或 DMARC 已配置正确。

DMARC 需要通过且与 Header From 对齐的 SPF 或 DKIM 路径；不是要求 SPF 与 DKIM 必须同时通过，也不能把任意 Provider 域的 DKIM pass 等同于发件域对齐。换 Provider 不能只检查 SMTP AUTH 成功。[RFC 7489 §3.1](https://datatracker.ietf.org/doc/html/rfc7489#section-3.1)

## 已有身份验收记录

以下仅汇总 Phase 3.5 的历史收件证据，不代表重新查询了当前 DNS。两个发件域分别使用不同 Provider，不构成“同一发件域的双 Provider 互备”验收。

| 发件域 × Provider | 通道 | 历史证据 | 仍需确认 |
| --- | --- | --- | --- |
| atland.icu × SpaceMail | 465 / 587 | Gmail/iCloud SPF、DKIM pass；QQ DKIM pass | Gmail 未列 DMARC，iCloud none，QQ none(permerror)；退信控制与信封改写未验收 |
| unisso.net × PurelyMail | 465 / 587 | Gmail/iCloud SPF、DKIM、DMARC pass | QQ 发件域签名描述及 DMARC permerror 差异；退信控制与信封改写未验收 |
| atland.icu × PurelyMail | 未验收 | 无 | 账号域权限、信封、签名和对齐均需独立验收 |
| unisso.net × SpaceMail | 未验收 | 无 | 账号域权限、信封、签名和对齐均需独立验收 |

依据与局限见 [真实 Provider 验收](real-world-validation.md)。学校邮箱未独立核验，Outlook 未覆盖，预先签名的原文尚未完成真实 DKIM 保持验收。收件箱与垃圾箱分类不驱动 retry/failover。

每增加一组发件域与 Provider，记录：操作者/时间、Provider 配置 ID、Header From、实际 MAIL FROM/Return-Path、允许发件域、收到邮件中的 SPF 身份与结果、DKIM `d=`/`s=` 与结果、DMARC 对齐与结果、Gateway ID/attempt/recipient，以及私有原始证据的位置和校验值。密钥、密码和个人收件地址不写入公开报告。真实验收需独立授权账号、目标和发信数量。

## Phase 7 的可信通道设计

先验证通道，再实现解析与自动规则。DSN 内容本身可以伪造，邮件里出现 Original-Envelope-Id、Reporting-MTA 或原始 Message-ID 都不是可信来源证明。[RFC 3464 §4.1](https://datatracker.ietf.org/doc/html/rfc3464#section-4.1)

优先评估 Provider 提供的认证反馈 API/签名 webhook：使用独立的读取凭证或验证签名、时间窗口与重放标识，再要求反馈对象归属于已配置的 Provider 账号。尚未核实 SpaceMail/PurelyMail 是否提供满足这些要求的通道，不作可用性承诺。

若使用专用退信邮箱，先证明邮箱和退信域由运行方控制，测试 Provider 是否保留 MAIL FROM、允许哪些域以及实际退信进入哪里。通过认证 IMAP/API 读取只能证明“读到了这个邮箱”，不能证明来信没有伪造。必须结合不可猜测的、绑定 attempt/recipient 的反馈令牌，以及经验证的反馈来源；无法取得足够证据时仅进入人工待查。若 Provider 改写信封且不提供可靠关联，保持自动处理关闭。

计划采用单独的反馈令牌密钥（支持 key ID 与轮换），令牌不公开邮箱或原始邮件，绑定 Provider、Gateway ID、attempt、recipient、有效期及用途。它只是关联条件，不单独作为真实性证明。是否采用 VERP、Provider 元数据或 SMTP ENVID 取决于实际通道能力；改变信封发件人需要单独设计与验收，不偷偷改变 transparent mode。

## 关联与状态处理设计

未来消费者先持久化原反馈、来源验证结果和去重键，然后按以下条件处理：

1. 来源认证满足所选通道约束；无认证、签名错误、过期或来源账号不符的反馈隔离，不能写名单。
2. 关联唯一的 Provider / Gateway ID / attempt / recipient；核实该收件人实际属于该次尝试。原 Message-ID 可能为空或重复，只作为辅助线索。
3. 检查反馈事件 ID 与内容摘要。重复反馈不重复写事件或延长抑制；同一 ID 内容不一致、收件人被改写或关联到旧重试结果有歧义时转人工。
4. 区分可信地址无效与临时错误、内容/策略拒绝、账号配置或配额问题。不能把所有 5xx、所有 `Action: failed` 或任意 free-text 错误当成长期无效地址。
5. 在同一数据库事务中提交反馈事件、独立反馈状态和经验证的抑制决策。保留此前 SMTP_ACCEPTED 的 attempt 事实；未知反馈不自动解除 UNKNOWN，也不自动重发。

反馈的 `BOUNCED` 和 SMTP 接受是不同层次的证据。没有退信不意味着 DELIVERED。若同一地址存在已知更新的成功证据，不能让迟到且有歧义的旧反馈直接误封，规则需由故障样本与测试确定。

## 启用自动处理前的验收

需要覆盖可信硬退信、临时退信、伪造签名、缺失/错误令牌、重复和内容冲突、多收件人、错账号、错 attempt、迟到和转发、退信邮箱改写、消费者崩溃/重启、审计失败与同一邮件已发生的新尝试。验证失败只允许人工处理，不能以“解析成功”替代信任判断。

该设计不扩大 Phase 5A 范围：本阶段只有操作者通过本地 CLI 明确添加记录，普通邮件内容与远端 5xx 均没有自动写 suppression 的权限。
