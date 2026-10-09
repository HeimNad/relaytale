# Phase 8F.5b.2 · 有界草稿与精确修订发布

2026-10-08。本阶段提供运营 CLI：保存草稿修订、列出元数据、导出指定修订供本地审阅，以及发布精确修订。草稿不参与网站发信目录，不改变已入队 EML 或已发布内容。管理 HTTP API 与浏览器工作台尚未增加。

## 保存与审阅

```sh
relaytale save-template-draft --api-key-id WEBSITE_CREDENTIAL_UUID \
  --expected-revision 0 --actor operator_name --reason 'initial draft' < notice.json
relaytale list-template-drafts --api-key-id WEBSITE_CREDENTIAL_UUID
relaytale show-template-draft --api-key-id WEBSITE_CREDENTIAL_UUID \
  --id notice --revision 1 > notice-reviewed.json
go run ./examples/template-preview -template notice-reviewed.json < example-variables.json
relaytale publish-template-draft --api-key-id WEBSITE_CREDENTIAL_UUID \
  --id notice --revision 1 --version EXACT_REVIEWED_CONTENT_VERSION \
  --actor operator_name --reason 'reviewed notification'
```

新建使用 expected-revision=0；编辑填写当前修订号，保存后追加下一修订，不覆盖历史。模板 ID 从 JSON 定义取得，每个 ID 只有一个当前草稿，语言变更也在同一修订序列中。可省略内容 version 自动计算；编辑已导出的对象时，删除原 version 或提供与新内容匹配的版本，否则拒绝保存。所有定义沿用已发布模板语法与变量验证，因此无效或未完成语法的内容需要留在本地编辑器中。

列表最多返回八个当前草稿的 ID、修订、内容版本、字节数、时间和审计 ID，不返回正文。show 命令必须指定修订，会导出完整源码，仅面向持有运营数据库凭证的人员；导出文件应保留在私有目录。预览仍是无投递的 JSON 源码预览，不渲染浏览器或访问外部资源。

相同规范化内容在预期当前修订，或上次成功保存的预期修订号下重试，返回原修订且 replayed=true，不增加历史。若另一位操作者已保存不同内容，返回明确修订冲突，需要重新导出、审阅后再编辑。不能通过刷新预期号自动覆盖他人修改。actor/reason 是有界审计标签，不替代数据库权限。

## 精确发布

发布要求当前最新草稿修订号及其完整内容版本，两者必须与审阅对象一致。导出后若有并发编辑，发布旧修订会拒绝；身份行锁让“检查当前修订 → 读取内容 → 写发布审计与不可改写版本”在同一事务中完成，不在检查后另起事务加载可变内容。

首次发布审计增加 draft_revision，关联同身份、ID、修订、内容版本与完整定义摘要。重复发布既有相同定义沿用原版本与审计，不追加第二条发布记录；若原先通过文件命令发布，该记录仍保留原来源，不能把重试描述成新的草稿发布。草稿发布也沿用活跃/历史限额与退役规则，相同内容版本不会因草稿重新发布而复活。

成功发布不等于激活：仍需为网站身份显式启用 use_published_templates 并重启。草稿更新和发布不热替换运行快照；已接受邮件继续用固化存档。人工审阅通过导出和本地预览完成，本阶段没有独立审批人状态或双人审批机制。

## 历史、容量与恢复

数据库草稿按身份/ID/修订不可改写，UPDATE/DELETE 由触发器拒绝。保存审计与定义事务提交，审计不包含正文或变量样例。每身份最多八个草稿 ID，当前修订定义合计 64 KiB，全部修订总计最多 64 条、512 KiB；每条输入及规范化定义最多 32 KiB。较大的旧修订计入历史预算，当前预算只计算各 ID 最新修订。草稿预算与已发布目录预算分别计算，不允许无限编辑历史；删除、归档与重置修订序列尚未提供。

读取源码、编辑和发布前校验该身份草稿的连续修订、定义摘要、内容版本、保存审计与预算。发布版本若声明草稿来源，启动及恢复还验证对应来源记录与保存审计。恢复逐条验证全部草稿历史，备份包含草稿表与审计；伪造或缺失来源拒绝验证，保留恢复启动门槛。没有草稿时原目录和 MIME 行为保持兼容。

新增 00027_template_drafts.sql。实际升级按 9B.1 停止写入并显式离线迁移，再按 9B.3 为新表更新运行账户授权，新旧程序不同时运行。旧 schema 备份用匹配版本恢复后再升级。本阶段未迁移真实部署数据库。

## 验收

完整容器 make ci 通过：格式检查、go vet、构建、全量竞态测试与隔离 PostgreSQL 集成回归（242.656 秒）。定向回归通过（11.560 秒），覆盖并发编辑冲突、重复保存/发布去重、旧修订与错误内容版本拒绝、草稿不参与发送、审计失败回滚、64 条历史限额、当前草稿容量、身份隔离、受限运行账户及 pg_dump/pg_restore 恢复源码。

最终完整回归还包含“发布记录关联缺少合法保存审计的草稿时拒绝启动”的测试。初次定向测试的历史限额夹具修改正文后遗漏声明变量，被正确拒绝；修正夹具保留变量使用后通过，未放宽模板验证。本机 API/CLI 测试及暂存差异检查通过。

仅隔离 PostgreSQL 与 Fake SMTP，不发送真实邮件，未迁移实际部署数据库。用户原有 .gitignore 修改未纳入阶段提交。

## 下一段

管理 API 与工作台可复用这些有界操作、修订冲突及来源校验；随后做浏览器预览与显式发布界面。8F.5b 尚未完整完成，不宣称已有可视化编辑器或热发布。

后续更新：管理 API 与工作台编辑、发布、退役已在 [8F.5b.3](phase-8f5b3-template-management.md) 完成；上文“下一段”描述本阶段完成时的范围。
