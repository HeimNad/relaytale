# Web UI

工作台提供邮件与收件人状态、事件时间线、Provider 配置/健康/预留用量、地址抑制及 UNKNOWN 人工处置。入口为 `/console/`，默认关闭。静态资源内嵌 Go 二进制，生产运行不需要 Node，也不请求外部字体或 CDN。

## 启用

1. 选择实际访问的 origin，例如 `https://relay.example.com`。只包含协议、域名和必要端口，不含路径或末尾斜杠；反向代理应保留 Host。
2. 为工作台生成独立密码哈希，密码长度 16–1024 字节。命令不会连接数据库，也不回显输入密码。下面示例在 Bash 中隐藏输入：

```bash
read -r -s -p '工作台密码: ' RELAYTALE_WEB_PASSWORD
printf '%s' "$RELAYTALE_WEB_PASSWORD" | relaytale hash-web-password --password-stdin
unset RELAYTALE_WEB_PASSWORD
```

3. 把哈希写入本地秘密配置。以下为 `.env` 的结构示例，替换占位符；**JSON 外的单引号用于保护 Argon2 哈希中的 `$`，避免 Compose 插值**。

```dotenv
WEB_UI_ORIGIN=https://relay.example.com
WEB_UI_USERS='[{"id":"alice","role":"admin","password_hash":"<上一步输出的完整哈希>"}]'
```

4. 保留有效 `RELAYTALE_MASTER_KEY`，用于 Provider 凭证写入，重启应用后访问对应 origin 的 `/console/`。HTTPS 由现有反向代理终止。不要为测试改动既有数据库/卷名或删除数据。

只在 localhost、127.0.0.1 或 ::1 origin 允许 HTTP，供本机开发；公网必须配置 HTTPS。应用检查明确配置的 Host/Origin，不信任客户端提供的转发头来推断安全来源。origin、用户配置必须同时提供；错误配置启动失败。

## 身份与会话

工作台账户与 SMTP、管理 API Bearer 令牌、未来 HTTP 发信 key 相互独立。最多 32 个账户，角色仍为 viewer/operator/admin；权限与管理 API 完全相同。操作者从服务端会话确定，不能在表单中冒充。

浏览器提交密码换取随机短期会话。应用不把密码或长期管理令牌写入 localStorage/sessionStorage；浏览器自身的密码管理器由使用者控制。HTTPS 会话使用 `__Host-`、HttpOnly、Secure、SameSite=Strict、Path=/ cookie，服务端只存会话 ID 的 SHA-256。CSRF 值仅保存在页面内存；每个写请求需匹配 origin 和 CSRF。

会话最长 30 分钟，闲置超过 15 分钟失效。前端也会到期清除页面数据；服务端判断为准。重新登录会轮换当前 cookie 对应的旧会话；退出在服务端立即撤销。账户配置修改或进程重启使全部会话失效。当前会话保存在单进程内存中，最多 256 个；多实例、共享会话、MFA、登录审计及热更新撤销尚未实现，不能把本阶段当完整企业身份系统。

登录只允许一个额外 Argon2 计算并发（64 MiB），独立于 SMTP 认证槽；部署内存需额外预留该工作量及 GC/运行时余量。按 TCP 对端 IPv4 / IPv6 /64 限制登录：突发 5 次、每 30 秒恢复一次；来源状态上限 1024，十分钟闲置回收。应用不信任 X-Forwarded-For，因此反向代理/NAT 后的用户可能共享额度。此设计限制资源消耗，不承诺分布式攻击下的可用性。

浏览器与 Bearer API 共用最多 8 个管理操作并发及原有数据库事务规则。浏览器 cookie 不能用于 Bearer API 路径，Bearer 凭证不能替代浏览器会话的 CSRF 检查。

## 使用边界

- 邮件列表筛选明确仅针对当前页；分页不是总数/实时订阅。详情可继续加载更多收件人、尝试与事件。
- 事件从 append-only ledger 读取。SMTP 已接受不等于实际收到；UNKNOWN 不自动重发。
- UNKNOWN 重新排队必须勾选重复风险确认并填写理由；人工“已确认收到”只记录操作者判断，不改写原 SMTP 尝试。
- 修改 Provider 使用展示的 revision；发生冲突需刷新核对，不能盲目提交。在途尝试不因停用或轮换密码而撤销。额度显示的是预留尝试单位，不是入箱数。
- 抑制添加/解除复用全局互斥与审计；解除不自动重发历史记录。
- 原始正文、HTML、附件与外部图片均不提供浏览器预览。主题、地址、名称按文本节点显示，CSP 禁止外部脚本和框架嵌入；这不是可渲染原始邮件的沙箱。
- 返回 503/断网可能发生在事务提交之后。先查询对象状态、版本和尝试，再决定后续处理。

SMTP 账户、主密钥轮换和部署参数继续通过配置/CLI 维护；本轮没有不可用的占位设置页面。完整邮件内容预览需要另行设计隔离方案。

## 验证与开发

Go 检查沿用 `make ci`；Node 仅用于格式和浏览器测试：

```sh
npm ci --ignore-scripts
npm run check:ui
npx playwright install chromium
TEST_DATABASE_URL='<隔离测试数据库 URL>' sh scripts/test-browser.sh
```

测试脚本启动本机 18080 端口、独立临时 schema 和 Fake SMTP，完成后停止测试进程；不要传生产数据库。浏览器测试覆盖真实查询到人工操作，并额外模拟空结果、503 和前端时间推进检查失败/过期界面。截图和 trace 放在被 Git 忽略的 test-results。

本地若下载浏览器受网络限制，可通过 `PLAYWRIGHT_CHROME_PATH` 指向已安装 Chrome；CI 使用 Playwright 固定版本的 Chromium。测试账户和合成邮件只存在于隔离 fixture 中，不随产品创建默认账户。
