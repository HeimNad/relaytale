# Phase 8F.5b.4：模板渲染预览

2026-10-08。工作台增加示例变量、主题/文本与受限 HTML 结构预览，复用实际发送的模板渲染器。预览不会保存样例、写审计、创建 EML、入队、占用 Provider 配额或发送邮件。

## 精确预览接口

operator/admin 可 POST `/admin/v1/templates/{id}/drafts/{name}/preview`，浏览器使用既有会话/CSRF 适配入口。viewer 不可预览，因为结果包含源码派生内容。管理角色仍是全局可信运营角色，不是网站发送身份。

```json
{"revision":1,"version":"<完整内容摘要>","language":"zh-cn","variables":{"name":"示例客户"}}
```

读取指定不可改写草稿，核对版本、语言和当前修订；旧修订返回 409，版本/语言/样例错误返回 400。不存在身份或修订返回 404，其他错误保持通用错误响应。新修订可在预览检查后出现，预览不持锁等待人工处理；后续发布仍按最新精确修订在事务内校验，不能用预览绕过并发保护。

响应是 no-store JSON，包含 dry_run=true、identity、revision、id、version、language、subject、text、html 与 rendered_bytes。字节数是 UTF-8 主题、文本和 HTML 内容合计，不是完整 MIME 或附件大小。JSON 返回原渲染 HTML，仅供可信管理客户端，不能直接插入业务网站 DOM。

请求封套仍限 16 KiB；变量最多 16 个，每值最多 1024 字节，总值 8192 字节，必须完整匹配声明；沿用重复字段、未知字段、非字符串和主题注入拒绝。文本/HTML 输出各受既有 16 KiB 限制，主题 256 字节，没有语言回退。使用现有 PreviewTemplateJSON 与发送渲染器，不维护另一套字符串替换规则。

## 浏览器隔离

页面在本次会话填写样例，不使用本地存储。输入变化立即清空预览并禁止由预览进入发布；慢请求结果以请求序号及页面 generation 丢弃。返回目录或退出会清理页面结果。发布仍展示精确源码并要求人工确认，不代表双人审批，也不改变明确启用与重启规则。

HTML 在未接入页面的 inert template 中解析，然后重新构建封闭的文字/标题/列表/表格标签集合。所有属性与样式被移除，图片变为隐藏提示，链接只保留文字；脚本、表单、框架、媒体、SVG/MathML 等活动内容整个移除。重建最多访问 4096 个节点、深度 64，超出部分省略。因此这是简化结构预览，不是保真邮件客户端模拟，也不覆盖附件或实际投递。

重建结果进入无任何 allow 权限的 sandbox iframe，srcdoc 顶部 CSP 为 default-src 'none'、base-uri 'none'、form-action 'none'，无脚本、同源权限、链接导航或网络资源。父页面仅新增 frame-src 'self' 以允许本地 srcdoc，不放宽脚本、样式或连接策略。隔离设计参考 [MDN srcdoc](https://developer.mozilla.org/en-US/docs/Web/API/HTMLIFrameElement/srcdoc) 与 [iframe sandbox](https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Elements/iframe)。

沿用 Impeccable Operate 指导和现有工作台视觉语言；按桌面/手机批量检查，调整变量输入与操作间距，不更换整体设计。

## 验收

完整容器 make ci 通过：格式检查、go vet、构建、全量竞态与隔离数据库回归（集成 265.380 秒）。最终全部六项浏览器测试通过（按规格隔离认证预算），UI 格式及暂存差异检查通过。定向集成已通过（0.689 秒），覆盖 viewer 拒绝、版本/语言错误、缺失及超长变量、旧修订拒绝、预览不增加邮件或审计记录。首轮真实 Chrome 浏览器预览测试通过（9.9 秒），验证恶意脚本文本转义、活动标签移除、无外部请求、样例修改清空以及并发修订冲突。

使用隔离 PostgreSQL 与 Fake SMTP，未发送真实邮件、未迁移实际数据库。没有新增依赖或迁移，用户原有 .gitignore 修改不纳入提交。当前浏览器验收使用 Chrome，其他浏览器和真实邮件客户端效果不作为本阶段已验收范围。
