# Phase 9B.3：运行、迁移与恢复数据库权限

日常 Gateway 和数据库业务 CLI 必须使用受限运行账号；超级用户、数据库/schema/表/函数所有者、拥有其他角色成员关系的账号，以及可修改迁移历史的账号被启动检查拒绝。没有开关绕过，也不回退到 POSTGRES_USER。本阶段收紧数据库权限；密钥版本及轮换仍为 9B.4，不能据此宣称整体生产就绪。

## 账号与连接

| 账号 | 责任 | 权限与边界 |
| --- | --- | --- |
| POSTGRES_USER | PostgreSQL 容器引导、创建角色及旧库所有权转移 | 不传入 Gateway；保管为管理员凭证 |
| relaytale_runtime | Gateway、投递、管理 API、采集/维护/诊断 CLI | 业务表 SELECT/INSERT/UPDATE/DELETE；序列 SELECT/USAGE；业务函数 EXECUTE。迁移历史只读，events/maintenance_audit 仅读和追加，无 CREATE/TEMP/TRUNCATE/TRIGGER/所有权或其他角色成员关系 |
| relaytale_migrator | 显式离线迁移、应用授权配置 | 专用数据库/schema/对象所有者，非集群管理员，无 CREATEDB/CREATEROLE/SUPERUSER/REPLICATION/BYPASSRLS |
| relaytale_recovery | 停写备份、空目标恢复与验证 | 独立登录和密码；仅获 SET 到 migrator 的权利，默认在目标数据库切换为该所有者，恢复对象保持一致所有权 |

恢复账号具有与迁移账号相同的数据库所有者能力，并非只读备份账号；两者分开凭证以减少日常分发与便于撤销，未实现命令级 SQL 权限隔离。连接使用 RUNTIME_DATABASE_URL / MIGRATION_DATABASE_URL / RECOVERY_DATABASE_URL。普通服务只收到 RUNTIME_DATABASE_URL 展开后的 DATABASE_URL，ops profile 的 migrate / recovery 服务各接收自己的连接。配置文件仍含敏感凭证，保持私有且不提交 Git。

check-db-permissions 只读检查 schema 与运行权限，不登记占用，可在启动前检查。检查不是业务权限完整性证明或恶意 DBA 防护；真正拒绝 DDL/迁移历史写入的是 PostgreSQL ACL。当前应用含在线管理与清理，需要广泛业务 DML，不提供数据库直连的租户隔离，不能保证运行凭证泄露后业务数据不被篡改。

## 首次部署

以下以默认数据库 relaytale 为例；修改过 POSTGRES_USER / POSTGRES_DB 的安装必须替换为真实值，同时修改三个连接 URI。密码使用独立随机值，URI 中的特殊字符必须百分号编码。不要把迁移/恢复密码复制到运行账号。

1. 复制 .env.example 为私有 .env，设置引导管理员密码。启动 `docker compose up -d postgres`。
2. 以管理员进入本地 PostgreSQL：`docker compose exec postgres psql -U relaytale -d relaytale`。先创建三份独立角色（已有角色不要盲目重建或覆盖）：

```sql
CREATE ROLE relaytale_runtime LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE relaytale_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE relaytale_recovery LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT relaytale_migrator TO relaytale_recovery WITH INHERIT FALSE, SET TRUE;
ALTER ROLE relaytale_recovery IN DATABASE relaytale SET role TO relaytale_migrator;
```

使用 psql 的 `\password relaytale_runtime`、`\password relaytale_migrator`、`\password relaytale_recovery` 分别设置密码，避免密码进入 SQL 历史或参数。填入 .env 的三个 URI，主机为容器网络内的 postgres。退出 psql。

3. 在专用 RelayTale 数据库内转移所有权；脚本不创建角色、不设置密码、不操作其他数据库：

```sh
docker compose exec -T postgres psql -U relaytale -d relaytale \
  -v owner_role=relaytale_migrator < docker/postgres/transfer-owner.sql
docker compose --profile ops run --build --rm migrate
docker compose exec -T postgres psql -U relaytale_migrator -d relaytale \
  -v runtime_role=relaytale_runtime -v recovery_role=relaytale_recovery \
  < docker/postgres/runtime-grants.sql
docker compose run --build --rm relaytale check-db-permissions
```

上述容器内 psql 使用该 PostgreSQL 容器现有本地认证方式；远程数据库用其正式 TLS/认证配置连接，不改为 trust。拥有 Docker 控制权本身是高权限，账号拆分不会隔离主机管理员。

4. 首次 localhost 开发环境按 README 生成证书，然后启动服务。普通数据库业务 CLI 继续通过 relaytale 服务执行；迁移使用 migrate 服务。不要再用 relaytale 服务执行 migrate / backup / restore，它会拒绝运行凭证。

## 已有数据库升级

保留原项目名、数据库名、卷名和 EML 根目录，先备份并停止旧 Gateway、采集器及全部维护进程。核对两类持久占用；遗留记录不能直接删除，按 9B.2 的人工流程处理。确认停止后才执行角色配置、专用 public 对象所有权转移和显式迁移，再应用 runtime-grants.sql、运行 check-db-permissions，最后启动新版本。

transfer-owner.sql 只转移目标数据库、public schema 中的应用表/序列/视图/普通函数；不使用跨数据库影响的 REASSIGN OWNED，不处理任意扩展、程序或自定义类型。仅支持专用 RelayTale 数据库；混用其他应用时先制定单独迁移方案。脚本使用独占迁移准入锁和占用检查，所有更改同事务完成，没有邮件或文件副作用。

runtime-grants.sql 同样要求离线准入与无遗留占用，同事务收紧 PUBLIC 的 CONNECT/TEMP/schema/函数权限，重新授予当前业务对象权限，撤销迁移历史写入、序列使用和两份追加日志的修改权限。旧有列级 grant 不会被表级 REVOKE 自动消除；权限检查发现历史列写入授权时应显式撤销并重新检查。意外 SECURITY DEFINER 函数会使授权脚本拒绝执行。

新对象不自动授予运行账号权限，每次迁移后重新应用授权配置并验收；默认权限按创建对象的角色作用，必须始终以 migrator 作为有效角色迁移/恢复。今后新增仅追加表、函数或更细角色时要同步评审配置；当前脚本对现有普通业务表统一授予 DML，不能用于混合数据库。脚本纯 SQL 事务无需网络/文件收尾，连接丢失后由 PostgreSQL 回滚或提交，仍须检查结果。

## 备份与恢复

通过独立 recovery 服务执行，按 9A 的真实停写要求和外部副作用对账：

```sh
docker compose --profile ops run --rm -v '<私有备份目录>:/backups' recovery \
  backup --offline --storage-dir /data/eml --bundle /backups/backup-001
```

恢复到已准备的空目标数据库，数据库所有者为 migrator；管理员为 recovery 登录授予目标 CONNECT，并为该目标设置默认 role=migrator。RECOVERY_DATABASE_URL 指向目标，同时 RUNTIME_DATABASE_URL / MIGRATION_DATABASE_URL 后续也改为目标。确保目标是独立空卷/空 EML 根，仍遵守原绝对路径和工作目录限制，绝不复用源数据卷作为恢复目标。

pg_restore 使用 --no-owner / --no-privileges，因此恢复之后**必须**作为 migrator 重新应用 runtime-grants.sql、验证恢复、检查受限权限。恢复闸门保持不动；权限验收不会解除 .restore-pending、启动 worker 或重试 UNKNOWN。管理员确认数据、密钥、投递对账之后，才按 9A 操作解除闸门并逐步恢复。

## 验收与资料

真实受限登录的 race 集成测试通过：Provider 写入、HTTP 入队幂等、Fake SMTP 投递、队列/触发器和占用释放可用；建表/临时表/建角色/切换高权限角色/关闭触发器/TRUNCATE/DROP/迁移历史写入/追加日志修改均被 PostgreSQL 拒绝。启动检查拒绝管理员、所有者及意外历史写授权。针对性测试耗时 2.656 秒。

真实二进制验收通过：旧库限定范围所有权转移、受限启动、拒绝管理员/所有者运行、受限在线诊断、拒绝运行账号迁移/备份、独立恢复登录备份/恢复/验证、恢复后重新授权和保留启动闸门。Compose 合成配置检查确认三类凭证独立，普通服务不含管理员、迁移或恢复凭证。最终 make ci 通过：格式、go vet、构建、全部 race 测试和隔离 PostgreSQL 集成测试通过（194.494 秒）；补充的函数所有权及非继承/不可 SET 的角色成员关系拒绝测试同时通过。暂存差异检查与离线敏感信息扫描通过。测试仅使用隔离 PostgreSQL 与 Fake SMTP，没有连接真实 Provider，也未改变现有部署账户。

权限来源与限制依据：[PostgreSQL 17 schema 权限](https://www.postgresql.org/docs/17/ddl-schemas.html)、[默认权限](https://www.postgresql.org/docs/17/sql-alterdefaultprivileges.html)。PUBLIC 默认授权和默认权限按对象创建者生效，不能只撤销某个登录的显式权限后便认为已隔离。
