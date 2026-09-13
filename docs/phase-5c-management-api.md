# Phase 5C · 最小管理 API

目标：为后续界面提供受控管理入口，复用 suppression 和 UNKNOWN 服务，保持 SMTP_ACCEPTED 与实际送达的区别。默认关闭，不提供 HTTP 发信或邮件正文/附件读取。

已实现：
1. 独立环境令牌配置、viewer/operator/admin 三档权限、严格 JSON 与有界请求；不用 cookie，不开放 CORS，拒绝带 Origin 的请求。
2. 消息、收件人、尝试、事件时间线、Provider 健康与滚动配额、suppression 分页查询；响应字段明确列出，不导出凭证、存储路径或原始调试日志。
3. Provider 配置、启停和 SMTP 密码轮换使用版本条件，状态与脱敏审计同事务；suppression 和 UNKNOWN 使用原有业务服务及并发条件。操作者来自令牌，不能由请求冒充。
4. PostgreSQL 集成测试覆盖权限、脱敏、并发冲突、重复操作和审计失败回滚；更新文档后建立阶段检查点。

Provider 停用只影响之后的领取，不能撤销已经领取或发出的 SMTP 操作；凭证轮换同样不替换在途尝试快照。管理请求不主动连接 Provider，避免把配置变成即时网络探测入口。


## 本地验收

隔离 Linux + PostgreSQL 执行 `make ci`：全仓库 gofmt、go vet、构建及完整竞态测试通过，集成测试 75.964 秒。新增覆盖：

- 默认禁用、缺失/错误令牌、只读/操作员权限、Origin 拒绝、actor 冒充、超大/重复/尾随 JSON、分页输入。
- Provider 创建重复冲突、同版本并发修改仅一方成功、密码轮换可解密且不出现在响应/审计中、审计失败时密码和 revision 原子回滚。
- suppression 添加/解除重复冲突，UNKNOWN 重试要求确认且受 suppression 阻断，人工处理后重复提交冲突；actor 来自令牌。
- suppression 添加/解除、UNKNOWN 处置的审计故障回滚，UNKNOWN 不留下孤立事件。
- 事件按时间加 UUID 分页，特意插入与 UUID 顺序无关的时间值；查询不暴露路径、原始调试信息和凭证字段。

实际进程 smoke test 也通过：隔离数据库上启动应用，已认证查询 200、缺令牌 401，worker 保持关闭；没有使用真实 Provider 或发送邮件。

接口、配置及剩余边界见 [管理 API 手册](management-api.md)。生产迁移互斥、主密钥版本/轮换与浏览器会话仍未完成；本轮验收不代表生产就绪。
