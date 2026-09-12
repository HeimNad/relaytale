# RelayTale 改名与兼容

产品名称统一为 **RelayTale**，Go 模块、命令、Compose 服务和镜像内用户使用 `relaytale`。入口为 `cmd/relaytale`，构建产物为 `bin/relaytale`，主密钥变量为 `RELAYTALE_MASTER_KEY`。规格说明书已改名；SMTP 协议、数据库表、原始 EML、UUID 与投递状态不因改名改变。

## 已有部署

不能直接把已有 PostgreSQL 用户、数据库或 Docker 卷改成新名字。PostgreSQL 的初始化变量不会重命名卷内已有角色与数据库，切换 Compose 项目名也会默认创建另一组卷。

已有本地部署在忽略的 `.env` 中保留 `COMPOSE_PROJECT_NAME=mailgate`、`POSTGRES_USER=mailgateway`、`POSTGRES_DB=mailgateway`，继续引用原有数据卷。主密钥只更换变量名，不改变值。新部署使用 `.env.example` 的 relaytale 名称。其他既有安装必须填写其实际项目、用户与数据库名称；不要照抄本地旧名称。

应用优先读取 `RELAYTALE_MASTER_KEY`，未设置时兼容 `MAILGATEWAY_MASTER_KEY`；显式空的新变量不会回退旧密钥。Compose 使用相同优先级。升级时应只保留新的变量名，并独立备份原密钥；不能生成一个新值代替已有密钥。

旧 Compose 服务名为 `gateway`，新名称为 `relaytale`。升级必须先通过旧配置停止旧发送进程并等待正常退出，再重建新服务与 Caddy；确认旧容器不再运行后单独移除旧应用容器。不要同时运行两个版本，不要执行删除数据卷的操作。本次代码改名不自动重启既有真实投递服务。

容器内用户名称变化，但 UID/GID 保持 10001，存档路径仍为 `/data`，无需更改数据所有权。旧的外部脚本需把应用命令和服务名更新为 `relaytale`。数据库操作可以通过容器内的 `POSTGRES_USER` / `POSTGRES_DB` 获取实际值。

## 历史与文件夹

已发送测试邮件的 `[MailGateway ef8ad30b66]` 主题、私有 EML、已有证书与 Git 阶段标签保留原样，它们是历史证据。普通文档和可执行示例采用新名称。

当前 Codex 工作区绑定 `mailgate` 的绝对路径，直接移动它会影响任务、索引与可能运行的工具，因此本次保留根目录名。可以在结束当前工作区后，将文件夹改为 `relaytale`，再重新打开项目并重建 CodeGraph 索引。Compose 的显式项目名保证文件夹变化不会隐式切换数据卷；仍需检查其他本地脚本和绝对路径。

## 范围

这是独立的命名检查点。Phase 5A suppression 草稿仍未接入发送链路，不属于本次已完成内容，不因改名被标记为验收通过。

## 改名验收（2026-09-11）

独立暂存快照通过 `go vet ./...`、全套 `go test -race -count=1 -timeout=8m ./...`，其中 PostgreSQL + Fake SMTP 集成测试耗时 55.541 秒。生产镜像构建成功；UID 10001、`relaytale license` 与打包 LICENSE 内容一致、关闭 SMTP/worker 后的数据库迁移与就绪检查均通过。Compose 配置校验、文档本地链接与日志导出脚本语法检查通过。没有执行真实外部发信。
