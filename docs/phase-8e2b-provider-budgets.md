# Phase 8E.2b · 共享 Provider 账号额度

2026-10-02 完成本地验收。为同一上游账号的多条 SMTP 配置增加共享预算组。网站提交预算与单线路 Provider 配额继续独立生效；本阶段不修改重试、UNKNOWN、健康熔断或发件域授权规则。

## 计费与边界

管理员显式把属于同一上游账号的配置加入同一组，例如 SpaceMail 465 隐式 TLS 和 587 STARTTLS。系统不会仅凭用户名/主机猜测归属，也不会自动移动现有配置。**必须将该账号的所有投递配置加入同一组**；未加入的线路仍按原规则计费，新建未分组线路不会自动继承账号预算。

组支持滚动 1 小时和 24 小时额度，单位为 **recipient_attempt（收件人投递尝试预留）**，0 表示对应窗口不限制，不是自然日、成功送达数或真实 Provider 账单。每次领取同时满足线路限额、组限额及既有健康/并发规则；只领取两层额度中允许的收件人数，剩余收件人留在队列。

| 事件 | 组额度处理 |
| --- | --- |
| 同账号换端口/安全模式、组内安全 failover | 共用余额，每次新尝试重新计费 |
| 重试、连接失败、存储失败、UNKNOWN、领取后崩溃 | 已提交的预留不退款 |
| 领取事务或最终提交失败 | 整个预留回滚 |
| 旧 Provider 加入组 | 自动包含该线路窗口内全部原有预留，不清零 |
| 禁用线路、换密码、改名称或限额 | 保留窗口内用量 |
| 组额度降低到已用量以下 | 阻止未来领取；不取消已领取/已发送的尝试 |
| HTTP 幂等重放/状态查询/Webhook | 不发起新的 SMTP 尝试，不消耗组额度 |

成员关系只允许从未分组变为加入一个组；重复加入、退组和移组都拒绝，数据库触发器同时保护。旧管理页面编辑线路时保留成员关系。更换上游账号或拆分组需要将来显式的迁移方案，本阶段不提供通过移组重置额度的捷径。

## 配置流程

升级前停止旧服务，执行新版本迁移，并保持 worker 停止，直至同一账号的全部线路分组完成。不要混跑旧 worker：旧代码不会执行共享组校验。默认不创建组，已有线路继续按原限额工作。

命令使用既有 `DATABASE_URL`，不会发送邮件；所有写入需要操作人和原因。示例中的变量由管理员填入：

```sh
relaytale create-provider-quota-group --name spacemail-account \
  --hourly-limit 100 --daily-limit 1000 --actor operator --reason '同账号双端口共享额度'
# 返回组 id/revision；使用实际返回值设置 GROUP_ID。
relaytale list-providers
# 读取两条线路各自的 id/revision；依次加入同一组。
relaytale attach-provider-quota-group --id "$PROVIDER_ID" --group-id "$GROUP_ID" \
  --expected-revision "$PROVIDER_REVISION" --actor operator --reason '同一上游账号'
relaytale list-provider-quota-groups --limit 100
```

对第二条线路重复 attach。`list-providers` 现在包含 `revision`、`quota_group_id`；分组清单返回组上限、revision、窗口用量和计费单位。清单分页使用最后一项的 id 作为下一次 `--after-id`，最多 1000 项。

修改组预算时使用 `configure-provider-quota-group --id ... --expected-revision ... --name ... --hourly-limit ... --daily-limit ... --actor ... --reason ...`。这是完整配置替换，省略 limit 会将该窗口设为 0。修改组策略和加入线路采用各自对象的版本号；版本冲突或提交结果不确定时，先读当前状态再决定是否重试。预算组不保存 SMTP 密码。

## 管理 API

沿用 `/admin/v1/` 身份体系，独立于网站发信令牌。viewer/operator/admin 均可读，只有 admin 可写；网页尚无专用分组表单，可使用 CLI 或管理 API。

| 方法与路径 | 输入/用途 |
| --- | --- |
| GET `/admin/v1/provider-quota-groups?limit=50&after=<UUID>` | 有界组列表，沿用已有管理分页格式 |
| GET `/admin/v1/provider-quota-groups/{id}` | 组上限、revision、窗口用量 |
| POST `/admin/v1/provider-quota-groups` | `id`, `settings:{name,hourly_limit,daily_limit}`, `reason`；可省略的 expected_revision 必须为 0 |
| PUT `/admin/v1/provider-quota-groups/{id}` | `expected_revision`, 完整 `settings`, `reason` |
| POST `/admin/v1/providers/{id}/quota-group` | `group_id`, Provider 的 `expected_revision`, `reason` |

策略创建、更新和成员加入均在事务内追加维护审计；审计失败则回滚。领取的 `QUOTA_RESERVED` 事件增加已分组线路的 `quota_group_id`；旧事件不改写。

## 并发和恢复

领取遵循 `message → provider → quota group` 锁顺序。即使两条线路分别持有自己的 Provider 锁，也必须争用同一组行锁；取得组锁后用新的数据库语句快照核对余额，不能沿用选择线路时的余额。持有组锁后不再领取其他 Provider 锁。成员加入也遵循 `provider → group`，组策略更新只锁组，不反向锁 Provider。

组内用量直接汇总成员已有的 `provider_quota` 预留账本，因此不会出现独立组账本漏扣或重复扣费；disabled 线路与 UNKNOWN 尝试也计入。预留窗口按数据库预留时刻计算。重启和备份恢复保留组、成员及原有预留账本，预算不会因进程内存清空而重置。

这是投递准入控制，不承诺真实邮件发出时间、上游计费同步或全系统公平调度；已领取尝试可能在降低限额或分组后继续发送。运维中需考虑组行锁串行化和窗口汇总开销，未做大规模账号组压力认证。消息流预算/公平调度及邮件有效期仍属于 8E.2c/8E.2d。

## 验证

已加入确定性并发测试：第三个事务持有组锁，让两条不同线路都完成最初选择并等待同一组，再释放锁；两者合计只能占用剩余 3 个收件人额度。覆盖历史预留加入组、隐式 TLS 与 STARTTLS 共用额度、滚动窗口、逐收件人拆分、组内重试/failover、UNKNOWN/崩溃不退款、提交失败回滚、管理权限/版本冲突/审计回滚、禁止退组/移组、网站预算与投递预算同时生效，以及备份恢复成员关系和组设置。

首轮测试发现夹具默认 STARTTLS 与隐式 TLS 场景不匹配，已修正测试线路安全模式。完整 `make ci` 已通过：格式检查、`go vet`、构建、全量竞态测试；隔离 PostgreSQL 集成测试用时 141.061s，包含恢复验证。全部网络投递使用 Fake SMTP，未发送真实邮件。
