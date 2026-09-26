# Phase 8C.3：只读存档目录核对

新增 `inventory-eml` 命令，从指定 UTC 日期目录查看磁盘条目与数据库可见引用的关系。不迁移数据库、不读取邮件正文、不写审计或队列、不移动和删除文件。

```sh
relaytale inventory-eml --storage-dir data/eml --date 2026-09-25 --limit 1000
```

连接使用 `DATABASE_URL`；存储目录可由 `EML_STORAGE_DIR` 指定。日期必须是有效的 `YYYY-MM-DD`。限额为 1–10000 个目录条目，默认 1000。目录不存在会报错，不当成“扫描成功且没有原文”。

## 输出

JSON 包含 `complete`、`truncated`、`next_after`、`scanned`、`items`；每项只有根目录内相对路径和分类，不含 MIME、主题或地址。

| 分类 | 含义 |
| --- | --- |
| REFERENCE_PRESENT | 文件名 UUID 或常见绝对/配置相对路径在 messages 中有引用；不代表原文完整或路径匹配 |
| NO_VISIBLE_REFERENCE_REVIEW | 此次查询未发现引用，仅供人工核查，绝非可删除文件 |
| UNRECOGNIZED_NAME | 不符合规范 UUID.eml 的常规文件，包括可能正在写入的临时文件 |
| NON_REGULAR_ENTRY | 符号链接、子目录或其他非常规条目，未读取内容 |
| ENTRY_UNREADABLE | 扫描时条目消失或无法读取元信息，需人工核查 |

任意消息状态的引用都会保留，包括 DELIVERY_UNKNOWN 和终态消息。查询失败直接中止，`complete=false`，不得把数据库不可用解释为无引用。

## 有界扫描与局限

每次只核对一个日期目录。8C.4 起支持分页：超过 limit 则 `truncated=true`、`complete=false`，成功页退出码仍为 0，使用 `next_after` 作为 `--after-cursor` 继续，直至 `complete=true`。实际读目录或数据库失败仍返回非零，不提供下一页游标。扫描限制与并发边界见 [分页说明](phase-8c4-inventory-pagination.md)。`complete=true` 仅表示本次页已到所选日期目录末尾，不代表整个存储已扫描或文件可以删除。

日期路径组件拒绝符号链接，文件元信息通过 os.Root 限定访问范围。数据库和文件系统不是同一原子快照；并发接收、提交、文件更名或特殊历史路径别名可能导致暂时未匹配。命令不会给出“安全删除”判断，也不通过文件年龄推断可删除。生产中核查应同时检查运行中的接收任务、配置的根目录和历史路径记录。

此前 `preflight-eml` 从数据库检查待投递原文的存在、长度、摘要及 CRLF；本命令从磁盘查看可见引用，两者方向不同。终态消息的全面内容校验、跨日期自动扫描、候选持久化与安全回收仍待实现，完整 8C 尚未核销。

## 验收

集成测试覆盖有引用/未匹配文件、临时文件、符号链接、目录限额、取消、非法日期和引用路径改变后仍保护消息 ID；确认原文与事件记录未修改。隔离 Compose 完整 `make ci` 通过，含格式、vet、build 和全量 race 测试；集成测试耗时 92.048 秒。实际 CLI 在隔离容器中对合成文件输出 `NO_VISIBLE_REFERENCE_REVIEW`，退出码为 0。`git diff --check` 通过。
