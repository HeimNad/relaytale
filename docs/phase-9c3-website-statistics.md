# Phase 9C.3：网站运营统计与逻辑容量盘点

新增网站结果接口 `GET /v1/statistics`，使用现有 HTTP 发信身份、只读额度、四个全局读取槽和每请求十秒上限。网站可统计自己的提交队列与逻辑原文容量，不需要取得管理凭证。本阶段不增加 UI、磁盘扫描、清理权限或 schema 迁移。

完整契约见 [OpenAPI](api/website.openapi.json)。

## 查询

```http
GET /v1/statistics?from=2026-10-01T00:00:00Z&until=2026-10-02T00:00:00Z
Authorization: Bearer <网站发信凭证>
```

from 必填且包含边界，until 必填且不包含边界。RFC3339 时间转换为 UTC，精度最高微秒，窗口最长 31×24 小时，until 不得超过数据库快照时间。不接受指定其他项目/身份的参数、未知或重复参数、请求正文或内容编码；只允许 GET，拒绝浏览器跨站请求，保持既有凭证接口的隐私响应头。

响应 schema_version 为 1，包含 environment、simulation、snapshot_at、from/until、message_count、messages_by_status、archives_by_state、logical_raw_bytes_by_archive_state 及 feedback_availability=NOT_EXPOSED。无匹配消息时计数为零、分组为空对象。

一个 API 身份就是本接口的统计范围。查询必须同时满足 api_key_id 和 source_type=api，不含 SMTP 提交，也不暴露全局 Provider、其他网站或隔离反馈信息。身份检查和业务查询在同一 REPEATABLE READ READ ONLY 快照中进行；数据库撤销或 token 轮换使后续请求失效，正在运行的已授权快照按原有读取契约结束。

## 统计语义

这是“窗口内提交的消息，在 snapshot_at 的当前状态”，不是窗口内发生的投递事件数。窗口内一封邮件从 QUEUED 变成 SMTP_ACCEPTED 后，重新查询会移动状态分组；一封多收件人邮件仍计作一封，不把消息状态当成逐收件人成功率。需要逐收件人结果时使用已有消息详情接口。

SMTP_ACCEPTED 只表示 Provider 接受，不能算作收件箱送达；DELIVERY_UNKNOWN 明确保留不确定状态。反馈仍返回 NOT_EXPOSED，不从没有退信推导成功，不将未核对 DSN 计入可信失败。

AVAILABLE、PURGE_PENDING、PURGED 的字节均来自 messages.eml_size。AVAILABLE 是记录上的可用容量，未检查文件存在/哈希；PURGE_PENDING 的文件可能已删除；PURGED 字节是原始邮件的历史逻辑大小，绝不是当前占用。物理磁盘、孤立文件、数据库大小、压缩效果及备份容量继续由独立盘点工具处理。本接口不触发原文保留或清理。

沙箱身份仅统计自己的沙箱消息，environment=sandbox、simulation=true，不能与生产投递表现混用。网站密钥轮换保留稳定身份，统计历史仍连续。

## 查询成本与错误

使用已有 messages_business_owner_time 索引限定身份与时间，最多读取 10001 条消息，每个分组只返回数据库状态枚举。窗口超过 10000 封返回 422 statistics_window_too_large，没有部分统计；缩小窗口后重查。多个窗口不应直接合成为精确历史状态快照，因为各请求的 snapshot_at 可能不同。超时、数据库故障或字节累加越界返回 503，不将错误冒充零用量。

无凭证/已撤销返回 401，非法窗口返回 400，方法错误返回 405，现有读取并发/速率限额返回 429。十秒请求上限约束数据库工作，不承诺大型历史库固定延迟；没有引入无限全库聚合或缓存凭证结果。

## 验收与下一段

测试覆盖真实本地 TLS HTTP 路由、不同身份及 SMTP 隔离、窗口两端边界、状态/逻辑字节、无敏感字段、无业务写入/文件修改/SMTP 连接、凭证撤销与轮换、沙箱标记、空窗口、未来时间、超过工作量上限不发布部分统计，以及查询与请求格式边界。定向 race 集成测试通过（3.575 秒）；完整 `make ci` 通过格式、静态检查、构建及全部 race 测试，集成测试用时 238.422 秒；OpenAPI JSON 与本地引用校验、暂存差异检查和离线秘密扫描通过。全部使用隔离 PostgreSQL 与 Fake SMTP，没有连接真实邮箱或发送真实邮件。

后续可在管理工作台展示这些统计，并补独立的跨项目容量盘点。物理存储验收、元数据归档、幂等退役和反馈关联保留仍是独立工作，9C 整体尚未完成。
