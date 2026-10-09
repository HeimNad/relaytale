# Phase 8F.5b.3：模板管理工作台

日期：2026-10-08。新增模板管理 API 与工作台，复用既有草稿、发布和退役事务；不改变 SMTP、队列或固化 EML。

## 接口与权限

以下路径位于 `/admin/v1`；浏览器通过既有 `/console/api` 会话与 CSRF 适配器访问。管理身份具有全局运营权限，不是网站租户身份；网站发送凭证不能用于管理 API。审计 actor 固定为已认证管理身份。

| 方法与路径 | 权限与返回 |
| --- | --- |
| GET `/template-identities` | 所有管理角色；网站身份的 ID、名称、enabled、environment；分页默认 50、最多 100 |
| GET `/templates/{id}/drafts` | 所有管理角色；当前草稿元数据，不含源码 |
| GET `/templates/{id}/drafts/{name}/revisions/{revision}` | operator/admin；指定修订源码，revision 必须是规范十进制 1–64 |
| POST `/templates/{id}/drafts` | operator/admin；保存草稿 |
| POST `/templates/{id}/drafts/{name}/publish` | admin；发布精确修订与内容版本 |
| GET `/templates/{id}/versions` | 所有管理角色；发布与退役元数据 |
| POST `/templates/{id}/versions/{name}/{version}/retire` | admin；确认退役 |

保存封套示例：

```json
{"expected_revision":0,"template":{"id":"welcome","subject":"欢迎 {{name}}","text":"你好 {{name}}","variables":["name"]},"reason":"创建欢迎模板"}
```

发布使用 `revision`、完整内容 `version` 和 `reason`。退役使用 `reason` 与 `acknowledge_retry_window:true`。客户端不能指定 actor。除身份分页外，模板作用域接口拒绝查询参数。不存在身份返回 404，角色越权 403，定义或预算无效 400，修订冲突 409，内部操作失败返回通用错误而不泄漏数据库内容。

仅草稿保存封套上限为 34 KiB，定义输入及规范化内容仍各限 32 KiB；其他管理写入保持 16 KiB。复用八个当前草稿、64 条历史修订等既有限额及五秒请求上下文、八个并行操作上限。没有无限历史或自动重试写入。

## 编辑、审阅与激活

工作台按网站选择目录，完整页面编辑主题、文本、HTML 源码、语言和声明变量。保存绑定读取时的 expected_revision；并发冲突保留本地输入，不自动刷新修订号覆盖他人修改。源码只用文本节点展示，不渲染 HTML，不加载图片或外部资源。

发布对话展示确切网站、修订、完整版本、变量与源码，并要求人工勾选已审阅。服务器仍以修订和版本事务校验为准；勾选不是独立审批记录，也不是双人审批机制。operator 可编辑，admin 才能发布和退役，viewer 只能读目录元数据。

发布不等于激活：仍需网站身份显式启用 use_published_templates 并重新启动服务。目录不是正在运行的快照证明。退役确认表示操作者声明已结束业务请求重试窗口，系统不能证明外部业务窗口已结束；退役仍不可逆并需重启生效。移除版本后旧业务请求可能拒绝，已接受邮件的固化 EML 不变。

## Impeccable 优化

按用户要求安装并阅读 [Impeccable](https://github.com/pbakaus/impeccable) v4.5.1 的操作、品质和润色指导，保留现有工作台设计语言。调整完整页面编辑布局、目录留白、手机按钮、成功提示颜色与手机退出入口。工具的 context 引擎未能在受限环境运行，采用文档指导和人工浏览器检查；没有宣称自动扫描通过。

首轮浏览器测试发现手机隐藏退出入口，修复后新模板流程通过。检查还发现目录意外显示 null，已过滤空节点并加入回归断言。新增测试累计登录触发既有来源限流，因此浏览器按独立服务分批验收，没有放宽生产认证限制。脚本支持指定测试文件以便隔离验收。

## 验收与边界

完整容器 make ci 通过：格式检查、go vet、构建及全量竞态与隔离集成测试（集成 274.704 秒）；模板定向集成通过（10.435 秒），UI 格式及差异检查通过。新增模板浏览器流程通过，原有四个浏览器流程分批通过；默认脚本隔离各规格启动测试服务。覆盖角色权限、源码隔离、审计身份、并发冲突保留输入、精确发布、退役确认、恶意 HTML 文本隔离，以及桌面/手机布局。使用隔离 PostgreSQL 和 Fake SMTP，不发送真实邮件、不迁移实际部署数据库。

本阶段没有新增迁移，但依赖既有 00027 草稿 schema 与运行数据库账户授权。渲染 HTML 的视觉预览、历史归档、运行快照激活状态展示仍待后续实施。

后续更新：受限 HTML 结构预览已在 [8F.5b.4](phase-8f5b4-template-preview.md) 增加；不是保真邮件客户端模拟。
