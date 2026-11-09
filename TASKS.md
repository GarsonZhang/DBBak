# DBBak 阶段任务清单

更新时间：2026-11-05

## 当前状态

阶段一 MySQL 全量、阶段二 PostgreSQL 全量与 SQL Server 原生全量均已真实验收（SQL Server 于 2026-11-02 完成异名恢复）。MySQL binlog、PostgreSQL WAL 时间点恢复脚本已真实验证，但尚未接入产品调度。远程 Agent 协议、自动清理和发行脚本有实现或测试，仍有控制端、服务运行和 Docker 验收缺口。对象存储（阿里云 OSS / S3 兼容）已完成代码实现与本地桩件测试，**真实 OSS 桶验收未执行**。

## 技术基线

- 服务端：Go 1.27.3，单实例模块化单体。
- 元数据：SQLite WAL，modernc.org/sqlite v1.61.0。
- 安全：bcrypt、HttpOnly 会话、AES-GCM 独立主密钥、Agent mTLS。
- 已验证数据库：MySQL 8.4.13 LTS；PostgreSQL 18.6。
- 前端：现有中文布局接真实 API，由 Go embed 提供。
- 代码布局：Go 模块根在仓库根目录（`go.mod`、`cmd/`、`internal/`、`embed.go`、`web/`）；`deploy/`、`packaging/`、`scripts/`、`DBBak_文档/` 并列存放。
- 测试边界：.test-runtime 隔离实例、独立恢复目标和测试目录。

## 已完成并验证

### 阶段一与二

- Go 服务、SQLite 迁移、认证、凭据加密和审计。
- 数据源、本地仓库、任务、调度、运行、文件、清理和恢复记录 API。
- MySQL 逻辑全量、SHA-256、真实备份和独立恢复通过。
- PostgreSQL 整实例 tar 物理全量、必要 WAL、system identifier 和独立集群恢复通过。
- 中文页面真实 API 接入及 Chrome 关键页面验证。

### 阶段三已验证脚本

- MySQL 8.4.13 binlog：全量记录真实坐标、ROW/FULL、FLUSH LOGS、隔离 dbbak_restore、目标标记与目标后标记重放边界通过。
- PostgreSQL 18.6 WAL：archive_mode、目标 LSN、真实归档段重放、目标后事务排除、system identifier 一致。
- 脚本均有哈希证据和明确超时；MySQL 与 PostgreSQL 源/恢复实例保持隔离。

### 阶段四与五已实现部分

- 远程 Agent：mTLS 客户端、一次性注册、CSR、fence、递增序号、去重、有界重试；单元和 httptest 通过。
- 独立全量自动清理：每天 05:00 后运行，复用预览、指纹、路径、哈希和最低份数保护；测试通过。
- Windows 原生服务（`dbbak.exe` 内置 `svc.Run`）与 Inno Setup 图形安装包、Linux systemd、Docker/Compose、跨平台构建脚本已交付并通过静态检查；WinSW 包装器已移除。
- Docker 镜像补装 `sqlcmd`：runtime 阶段从微软 RHEL 9 源装 `msodbcsql18` + `mssql-tools18` 并软链到 `/usr/local/bin/sqlcmd`，`INSTALL_SQLCMD=0` 可跳过；此前镜像只有 MySQL 客户端，SQL Server 连接测试固定报 `TOOL_MISSING`。**镜像未构建、容器未启动（本机无 Docker），该层未验证。**

### 手动删除任务与备份对象（2026-11-03）

- 新增 `internal/backup/removal.go`：任务、任务全部文件、单个对象三个删除入口，统一「先评估范围、再执行」两步模型。
- 新增 API：`GET /api/v1/jobs/{id}/removal`、`DELETE /api/v1/jobs/{id}`、`GET|DELETE /api/v1/jobs/{id}/artifacts[/removal]`、`GET /api/v1/artifacts/{id}/removal`、`DELETE /api/v1/artifacts/{id}`。
- 阻塞规则：启用中任务、活动运行、恢复链依赖一律拒绝；已校验或锁定对象需 `force=1` 加确认名称（查询参数 `confirm`，兼容 `X-DBBak-Confirm`），`force` 不绕过前三条。
- 安全约束：路径限定授权根目录，拒绝符号链接，已校验对象删除前复核大小与 SHA-256；文件逐个删除成功后才标记 `deleted`，任一失败返回 `409 PARTIAL_FAILURE` 且记录与文件保持原状。
- 修复（2026-11-03）：PostgreSQL 任务删除卡在「部分文件未删除」。原因是 `pg_basebackup` 中断留下空目录残留 `.run_<id>.attempt-1`，而删除路径走 `SecurePath(..., mustExist=true)` 只接受普通文件，记录永远无法清除。新增 `SecureRemovalPath`（允许目录、允许目标已不存在，仍复核根目录边界），`deleteArtifactFile` 只删除**空目录**、非空目录拒绝且不递归；磁盘文件已被外部删除时直接登记 `deleted`；失败 `message` 附带前几条「相对路径 + 原因」。
- 验证：`go test ./...` 全绿，新增 3 个用例（空目录残留可删、非空目录拒绝并保留内容、文件缺失登记删除）；在真实浏览器中对 `Vault 隔离 PostgreSQL 全实例` 复现并删除成功，磁盘空目录消失、任务与运行记录清除，无 JS 异常。
- 删除任务在一个事务内清理已删对象记录、运行日志、运行记录、清理预览与任务本身，并写入含任务名、备份类型、保留策略的 `job.delete` 审计；运行历史随任务删除，行为已在 API 文档说明。
- 前端：任务列表与「保留与清理 → 备份文件记录」新增删除入口；弹窗展示服务端评估的真实文件清单、阻塞原因与解除建议，强制删除需勾选并输入目标名称。
- 验证：`go test ./...` 全部通过（新增 8 个删除用例，覆盖确认、摘要不一致、恢复链依赖、任务需暂停、只删文件保留任务）；`testsrc/api` 路由用例覆盖任务级评估与启用中拒绝。端到端在 `127.0.0.1:38099` 隔离实例用 curl 实测：启用中删除返回 `409 REMOVAL_BLOCKED`；不带 force 返回 `409`；错误确认名返回 `422`；正确确认后 2 个真实文件从磁盘移除、任务/运行/对象记录归零、审计落库；篡改文件后强制删除返回 `409 PARTIAL_FAILURE` 且文件保留。
- 修复（2026-11-03）：删除弹窗把确认名称放进 `X-DBBak-Confirm` 请求头，而浏览器 `fetch` 的请求头值只接受 ISO-8859-1，中文任务名会直接抛 `String contains non ISO-8859-1 code point`，导致中文名任务无法删除。改为 `?confirm=` 查询参数（前端 `encodeURIComponent`），服务端优先读查询参数，`X-DBBak-Confirm` 保留给外部脚本并在值含百分号转义时解码；`api()` 增加请求头非 ASCII 的显式拦截，避免同类问题再次以浏览器原生报错形式出现。
- 验证：`go test ./...` 全部通过，`TestAuthenticationCSRFAndPersistentAPI` 覆盖「缺确认名返回 422」与「中文任务名确认后删除成功」；在 headless Chrome 中真实点选「删除 → 勾选强制 → 输入中文任务名 → 确认删除」，提示「任务、备份文件与运行记录已删除。」，无 JS 异常，列表计数回落到 8。
- 新增备份位置类型（2026-11-03）：用户指出「备份到数据库所在服务器」不该套存储位置模型——SQL Server 写备份的路径在数据库主机上，执行器永远看不到文件，旧模型的 `server_root` 同目录约束与本地哈希校验对外部数据库实例必然失败（用户真实任务 `tceDB` 已实测 `SERVER_PATH_REQUIRED`，即使配了 server_root 也会 `BACKUP_FILE_NOT_VISIBLE`）。按用户设计实施：`jobs` 新增 `location_kind`（`storage`/`database_server`）与 `server_dir`，迁移 6 重建 jobs/artifacts 使 `target_id` 可空（外键开关在事务外切换）。`database_server` 模式仅限 SQL Server：任务直接填服务器目录，sqlcmd 生成 BACKUP 脚本，msdb 的 LSN+backup_size 作为真实性凭证，产物标 `uploaded`；`LatestServerLocalArtifacts` 允许 uploaded 对象作差异/日志基线；删除与清理只清记录并提示「文件在数据库服务器上需人工删除」。NAS/S3 仍走存储位置模型，阶段二+再扩展。
- 验证：`go test ./...` 全绿；`TestServerLocalBackupJobValidation` 覆盖 MySQL 拒绝、缺目录/相对路径拒绝、与存储位置互斥、合法创建后 `target_id` 为空且列表字段正确；`TestRemovalDeletesEmptyDirectoryResidue` 等既有用例回归通过。真实库迁移 6 应用成功，历史任务 `tceDB` 保持 `storage` 模式无损。前端任务表单新增「备份位置类型」单选（非 SQL Server 数据源强制 storage），按类型切换「本地存储目录 / 数据库服务器目录」字段；任务列表按类型显示落点。
- 修复（2026-11-03）：数据库页把 `sqlserver` 引擎显示成「MySQL · 逻辑全量」，连接测试 toast 也硬编码「MySQL」前缀（用户对真实 SQL Server 192.168.10.198 测试成功却提示「MySQL Microsoft SQL Server 2019…」）。副标题补 sqlserver 分支「SQL Server · 原生全量」，toast 改为直接显示服务端返回的版本字符串。
- 提示：用户首个真实外部任务 `tceDB`（TCEDB → SQL Server 192.168.10.198）运行失败 `SERVER_PATH_REQUIRED`：存储位置「本地目录 D:\DB\bk」未配置 `server_root`。SQL Server 原生备份由数据库服务写文件，路径必须是实例可见的；当前兼容矩阵要求 `server_root` 与 `root` 指向同一实际目录（远程路径映射未实现）。已在答复中给出解除步骤（编辑存储位置填服务端路径）。
- 修复（2026-11-03）：二进制/脚本备份任务删除卡在「部分文件未删除（…attempt-1：目录非空（1 项））」。SQL Server 备份中断会在 `.run_<id>.attempt-1` 里留下 `backup.sql` 等中间产物，上一版「非空目录一律拒绝」的策略使这类 runner 自建工作目录永远清不掉，数据库里已积累 564 条同类 quarantined 记录。新增 `internal/backup/removal_residue.go`：`isAttemptResidueDir` 按 `^\.?run_<16-64 位 hex>\.attempt-<n>$` 精确限定范围，`removeAttemptResidue` 先整棵树校验（拒绝符号链接、设备/管道/socket、条目 >5000 或层级 >8 则整体放弃）再自底向上删除；未处于 `verified` 的残留目录走该路径，其他非空目录仍拒绝递归，正式备份对象不受影响。
- 验证：`go test ./...` 全绿，新增 5 个用例（非空残留整棵清理、非空普通目录仍拒绝、符号链接整体放弃、目录命名识别、条目类型判定；Windows 无权限创建符号链接时该用例 skip，改由 `residueEntryProblem` 构造 `FileMode` 覆盖分支）；经 API 实测删除 `SQL Server 2025 ????`：返回 `deleted_files=1`「已清理中断残留目录（1 个文件）」，磁盘 `.run_7749645…attempt-1` 目录消失，任务数 2 → 1。
- 遗留：`sqlserver-product/.run_7749645711870f9c1b1d068c.attempt-1.bak`（3.7 MB，旧路径布局遗留，无对应对象记录）未清理，属孤立数据，需人工确认后处理。
- 未验证：备份文件由测试直接写入 SQLite 构造，未走真实 mysqldump 产物；删除链路的恢复链依赖与 `409 PARTIAL_FAILURE` 分支未在浏览器中复现。
- 新增存储位置删除：此前「存储位置」只有写检查与编辑，无法移除已废弃目录。新增 `GET /api/v1/storage-targets/{id}/removal`（引用统计 + 阻塞原因）与 `DELETE /api/v1/storage-targets/{id}?confirm=<名称>`。语义为**仅解除登记**，不删除磁盘任何文件；仍被任务（`STORAGE_IN_USE_BY_JOB`）或备份对象（`STORAGE_HAS_ARTIFACTS`）引用时返回 409 并给出逐条解除建议。存储层 `DeleteStorageTarget` 在单事务内复查引用后删除并写 `storage.delete` 审计，避免检查后被并发任务穿透成外键错误；`StorageTargetUsage` 额外按任务反查清理预览。
- 存储位置类型扩展（2026-11-04）：新增共享目录（`kind=unc`）与 FTP（`kind=ftp`）。语义说明：SMB 是协议、UNC 是路径写法（`\\host\share`），程序按 UNC 访问时底层走 SMB，两者是同一能力，「共享目录（SMB）」即指 UNC 路径的远程目录，与本地目录共用落盘/哈希校验/清理逻辑。FTP 需要主机/端口/账号/密码/远程绝对路径，密码经 `KeyStore` 加密存入 `secrets`；迁移 7 为 `storage_targets` 增加 `host/port/username/secret_ref/passive`。
- FTP 实现（`internal/backup/ftpstore.go`，依赖 `github.com/jlaffaye/ftp v0.2.0`，纯 Go、MIT）：备份先落执行器临时目录 → 上传 → **整文件回读比对 SHA-256** 通过才标 `verified` → 删除本地副本；删除/清理先回读复核摘要再删远端文件；写检查为「上传-回读-SIZE-删除」。远端对象已不存在（550）与本地一致登记 `deleted`，不会让记录永久悬挂。SQL Server 原生备份写 FTP 被明确拒绝（其备份由数据库服务写盘）。
- 验证：`go test ./...` 全绿（`TestStorageKindValidation` 覆盖 unc 创建、FTP 缺主机/根目录非绝对/端口不可达均返回 422 且不入库）。真实端到端：本地 pyftpdlib 隔离 FTP（127.0.0.1:2121）→ 创建存储位置 201、写检查 200、错误密码 422；MySQL 隔离实例真实备份 2296 字节 → 上传 → 回读 SHA-256 一致 → `verified`，远端落点为 `/backups/vault_source/2026/11/run_xxx.sql`；删除该对象回读复核后移除远端文件，远端文件数归零。界面验证三种类型的字段显隐正确（FTP 隐藏授权根目录与服务端路径，SMB 隐藏 FTP 字段）。
- 待验证：FTP 仅在本地 pyftpdlib 上验证，未对生产 FTP 服务器（含 FTPS、非被动模式、中文文件名、大文件超时）验证；FTPS/显式 TLS 与断点续传未实现。
- 验证：`go test ./...` 全绿；`TestAuthenticationCSRFAndPersistentAPI` 新增覆盖「被任务引用时 409 且说明原因」「确认名不一致 422 且位置保留」「中文名称确认后删除 200」「重复删除 404」。真实浏览器点选验证：被引用位置弹窗显示「1 个备份任务…；仍有 938 个备份对象…」且 footer 仅「关闭」；无引用位置输入中文名称后删除成功、卡片消失、提示「存储位置登记已解除，磁盘文件未做改动。」，错误名称被前端拦下且弹窗保持打开，无 JS 异常。
- 新增数据源删除：与存储位置删除同一模式（预览 + 确认名删除）。新增 `GET /api/v1/sources/{id}/removal` 与 `DELETE /api/v1/sources/{id}?confirm=<名称>`，阻塞码 `SOURCE_IN_USE_BY_JOB` / `SOURCE_HAS_ARTIFACTS`。语义为**仅解除登记**：不连接、不改动数据库本身；`DeleteSource` 在单事务内复查引用后删除数据源并**连同 `secrets` 密文一并清除**（避免留下不可解密的孤儿凭据），写 `source.delete` 审计；弹窗明确提示凭据删除不可恢复、重新添加需重新录入密码。
- 验证：`go test ./...` 全绿；测试断言删除后 `SourcePassword` 返回 `ErrNotFound`（凭据确已清除），并覆盖被引用 409、确认名不一致 422、重复删除 404。真实浏览器验证：临时数据源删除弹窗展示引用统计与凭据警告，错误名称被前端拦下、弹窗保持打开，中文名称确认后删除成功、卡片消失，提示「数据源与加密凭据已删除，数据库未受影响。」，无 JS 异常。

### 版本 0.2.0-phase5（2026-11-05）

- **修复：安装失败时日志永远为空**（用户截图「服务注册或启动失败（退出码 1）」+「最近的安装日志：」空白）。根因是 `setupFromArgs` 打开 `--log-file` 后 `defer file.Close()`，而真正的原因是在它**返回之后**由 `run()` 里的 `log.Printf("错误: %v", err)` 写出的——句柄已关闭，写入静默失败。改为句柄存包级变量、由 `run()` 的 `defer closeSetupLog()` 收尾（非 Windows 提供同名空实现）。已验证：故意传非法 `--prefix` 时日志写入 `错误: --prefix 无效: 拒绝使用卷根目录 C:\`（修复前该文件 0 字节）。
- **安装器失败对话框增强**（`packaging/dbbak.iss`）：日志按 40 行显示（原 20 行）；日志为空时明确提示「安装程序在写出任何信息之前就退出了」，避免误判成日志损坏；新增 `PersistInstallLog`，把日志复制到 `<数据目录>\logs\setup-error.log`，因为安装器退出时 `{tmp}` 会被整体删除，客户只拿到退出码就无法排查。
- **修复：从旧安装目录升级被当成"别人的服务"拒绝**。`ownService` 原来只比对安装目录，用户换盘/改目录名（例如 `C:\Program Files\DBak` → `DBBak`）后重装时旧服务 ImagePath 仍指向老目录，`setup` 直接返回「服务已存在且不属于…」并退出 1；而 Inno 一侧早已按 ImagePath 停服并拍快照，两处语义互相矛盾。改为按可执行文件名识别产品（`dbbak.exe` 即本产品）后允许接管，`openExistingService` 会把 ImagePath 刷成新路径。
- 同时修掉 ImagePath 解析 bug：原逻辑先 `strings.Trim(…, "\"")` 去掉首尾引号，再找 `" ` 定位，含空格的路径会被截成 `:\Program`。新增 `exeFromImagePath`（带引号取引号内、不带引号按 `.exe` 截断），与 Inno 的 `GetServiceDir` 保持同一规则；新增 `TestExeFromImagePath` 覆盖 5 种写法。
- **端口占用前置检查**：全新安装时先试绑监听地址，不再等服务启动后靠 90 秒 readiness 超时才发现。失败时用 `netstat -ano` + `tasklist` 找出占用进程，报错直接写「被 mysqld.exe (PID 4312) 占用」。新增 `TestEnsureListenAddrFreeRejectsBusyPort` / `AcceptsFreePort`。
- **服务启动失败不再干等**：`waitReady` 每轮查询 SCM 状态，服务停在 `Stopped` 时立刻失败并带出服务日志尾部（端口、权限、主密钥问题的真实原因都在那里）。
- 修复首次安装无密码时静默不建管理员：`if !opts.NoAdmin && opts.AdminPass != ""` 内的 `if password == ""` 分支永远不可达，CLI 加 `--yes` 不传密码会装出「服务在跑但没有任何管理员」的实例。改为 `if !opts.NoAdmin`，密码为空即生成 24 位随机密码并打印一次。
- 版本号升至 `0.2.0-phase5`（`main.go`、`build.ps1`、`build-installer.ps1`、`build.sh`、`Dockerfile`、`compose.yaml`、构建指南版本基线）。
- 打包与验证：`go vet -p=1 ./...`、`go test ./...` 全绿（新增 3 个安装器用例）；`packaging/build-installer.ps1 -Version 0.2.0-phase5 -SkipTests` 通过。产物 `dist/0.2.0-phase5/`：`DBBak-Setup-0.2.0-phase5-windows-amd64.exe`（9.8 MiB，ProductVersion `0.2.0-phase5`）、ZIP 14.7 MiB、`SHA256SUMS` 已更新。
- 仍未验证：管理员会话下的真实全新安装与覆盖重装（本机沙箱禁用 `sc.exe`，无法注册服务）。失败时看 `<数据目录>\logs\setup-error.log`。

### 版本 0.2.0-phase4（2026-11-04）

- 管理员密码最小长度从 12 位降到 **6 位**（保留下限、不取消校验）。唯一来源 `internal/security.MinPasswordLength = 6`，`init-admin`、`setup` 均引用该常量；`HashPassword` 顺带补上 72 字节上限的显式拒绝（bcrypt 硬限制，原仅写在注释里）。`packaging/dbbak.iss` 因 Inno Pascal 无法引用 Go 常量，说明文案与 `Length(Password) <` 两处同步手改为 6。
- 同步文档与脚本：`scripts/start-dev.ps1`、`README.md`、`DBBak_文档/DBBak_构建发布运行指南.md`、`DBBak_文档/DBBak_部署说明.md`。
- 版本号统一升到 `0.2.0-phase4`（`main.go` `const version`、`build.ps1`、`build-installer.ps1`、`build.sh`、`Dockerfile` LABEL、`compose.yaml` image、构建指南版本表）。
- 打包：`packaging/build-installer.ps1 -Version 0.2.0-phase4` 通过（vet → test 全绿 → 构建 → 版本自检 → Inno 编译，约 1 分钟）。产物 `dist/0.2.0-phase4/`：Setup 6.3 MiB、ZIP 5.8 MiB。复核：`dbbak.exe version` = `0.2.0-phase4`；SHA-256 见 `SHA256SUMS`（Setup `37e36195…e617b9`，ZIP `c621e996…4b9b`）；独立重跑 ISCC 确认脚本语法无误。
- 构建坑：本机 PATH 里没有 go；`D:\Program\Go`（1.26.10）在 `GOTOOLCHAIN=auto` 下会去下载 1.27.3 而失败。**改用仓库自带 `.tools\go`（1.27.3）+ `GOTOOLCHAIN=local` 才能稳定构建。**
- 未运行：仍未做管理员会话下的真实安装/卸载验收；安装包无代码签名，SmartScreen 会提示。

### 版本 0.2.0-phase3（2026-11-04）

- 版本号统一从 `0.2.0-phase2` 升到 `0.2.0-phase3`（本次新增对象存储存储类型与迁移 8）。同步位置：`cmd/dbbak/main.go` `const version`、`packaging/build.ps1`、`packaging/build-installer.ps1`、`packaging/build.sh` 默认 `VERSION`、`deploy/docker/Dockerfile` LABEL、`deploy/docker/compose.yaml` image、`DBBak_文档/DBBak_构建发布运行指南.md` 的版本基线表。历史验证报告中的旧版本号属于历史事实，**未改**。
- 修复：`scripts/start-dev.ps1` 是唯一缺少 UTF-8 BOM 的 PowerShell 脚本（`dev-env.ps1`、`run-dev.ps1` 都有），Windows PowerShell 5.1 按 ANSI/GBK 读取导致中文字符串被截断，启动时报一串 `ParserError: UnexpectedToken`。已补 BOM，`start.cmd` 双击启动恢复正常。
- 打包与验证：`packaging/build-installer.ps1 -Version 0.2.0-phase3` 全流程通过（vet → test 全绿 → 构建 → 版本自检 → Inno Setup 编译）。产物 `dist/0.2.0-phase3/`：`DBBak-Setup-0.2.0-phase3-windows-amd64.exe`（6.3 MiB）、`dbbak-0.2.0-phase3-windows-amd64.zip`（5.8 MiB，含 dbbak.exe 与 deploy/windows 升级回滚脚本）、`SHA256SUMS`。复核：`dbbak.exe version` 输出 `0.2.0-phase3`；两个文件 SHA-256 与 `SHA256SUMS` 一致；二进制内确认包含 OSS 端点模板、校验文案与 `oss-endpoint-field` 前端字段。
- 未运行：安装包**代码签名缺失**（SmartScreen 会提示）；真实提权安装、服务注册、ACL 授权与卸载验收仍需管理员会话，本机未执行。

### 对象存储（阿里云 OSS / S3 兼容，2026-11-04）

- 新增存储类型 `kind=oss`，走 S3 兼容协议（`github.com/aws/aws-sdk-go-v2 v1.47.5`、`service/s3 v1.114.6`、`config v1.33.13`、`credentials v1.20.13`，Apache-2.0）。阿里云 OSS、MinIO、AWS S3 共用同一实现，不引入厂商专有 SDK。
- 迁移 8 为 `storage_targets` 增加 `bucket/region/use_ssl/path_style`；复用 `host/port/username/secret_ref`：`host` 承载端点、`username` 承载 AccessKey ID、`secret_ref` 指向加密的 AccessKey Secret，`root` 列改作对象键前缀（空表示桶根）。
- `internal/backup/ossstore.go`：`ObjectKey` 显式拒绝 `..` 越界段（`path.Clean` 会把 `../x` 折叠成 `/x` 而静默放行，对象存储前缀外可能有别人的数据，必须拒绝）；≤64 MiB 走单次 `PutObject` 并附带 `Content-MD5`，使 ETag 等价于整文件 MD5；更大文件走 16 MiB 分片上传递增，分片失败即 `AbortMultipartUpload`。
- **校验等级如实分级**：上传后用 `HeadObject` 复核长度；ETag 为 32 位十六进制时比对 MD5（内容级），带 `-N` 后缀（多段上传）或非 MD5 形态（部分 SSE-KMS）时只比对长度，并把「长度一致（多段上传 ETag 非整文件摘要，未比对内容哈希）」写进运行日志，不笼统宣称「校验通过」。本地 SHA-256 仍计算并入库，供恢复前复核与清理身份确认。
- 删除与清理：`ossHead` 先确认对象存在且长度与登记一致；≤4 MiB 对象额外回读重算 SHA-256 比对，超过则仅比长度（避免清理任务按流量拉取超大对象）；S3 `DeleteObject` 幂等，「对象不存在」登记为已删除，权限不足等错误保持记录原状等待重试。
- 写检查 `OSSWriteTest`：写探测对象 → `HeadObject` 读回长度 → `DeleteObject`，分别验证 `s3:PutObject` / `s3:GetObject`（或 `s3:ListBucket`）/ `s3:DeleteObject` 权限，缺任一项在保存时失败。
- SQL Server 原生备份写对象存储被明确拒绝（`UNSUPPORTED_LOCATION`），其备份由数据库服务写盘，执行器拿不到产物。
- 前端「存储位置」弹窗新增「对象存储（阿里云 OSS / S3 兼容）」类型：端点、Bucket、Region、AccessKey ID/Secret、对象键前缀、HTTPS、Path-style 六项；三类落点字段互斥显隐并同步 `required`，提交时只发送与当前类型相关的字段，避免把 FTP 端口带进 OSS 被服务端判为矛盾配置。
- 验证：`go build ./...`、`go vet ./...`、`go test ./...` 全绿。新增 7 个用例（对象键拼接与越界拒绝、ETag 三态解析、写检查零残留、上传内容一致、HEAD 失败必失败、删除后识别「不存在」、缺写权限必失败）；`TestStorageKindValidation` 扩展覆盖缺 Bucket / 缺端点 / 端点带路径 / 本地目录混入对象存储参数 / 端口不可达，全部返回 422 且不入库。隔离实例（`127.0.0.1:38177`）curl 实测五个校验分支返回码与文案符合预期，端口不可达分支真实发出 `PutObject` 请求并透出 `connectex` 错误（证明写检查不是空转）。
- 修复（2026-11-04）：`location-form` 内 FTP 与 OSS 两组字段同用 `name="username"` / `name="password"`，`form.elements.username` 因此返回**元素集合**，`value('username')` 取到空串，提交时 OSS 的 AccessKey 永远被服务端判成「必填缺失」。改为 FTP 用 `ftp_username` / `ftp_password`、OSS 独占 `username` / `password`，`required` 与提交取值按类型分派。这是 headless Chrome 真实填表提交才暴露的缺陷，单测与 curl 都覆盖不到。
- 界面验收（headless Chrome + CDP，脚本驱动真实点击与填表）：默认 local 时 FS 字段可见、FTP 与 OSS 组隐藏；切到 OSS 后 FS（含 `server_root`）与 FTP 全部隐藏，OSS 六项可见，端点/桶/AccessKey 为必填、对象键前缀**非**必填、HTTPS 默认勾选、Path-style 可选；填不可达端点提交后弹窗保持打开并透出 `PutObject ... connectex` 原文，列表无残留；切回 FTP 提交后正确透出 `连接 FTP 127.0.0.1:1 失败`，证明两组字段名已无歧义。全程无 JS 异常。
- 未运行：**真实阿里云 OSS 桶端到端未执行**（无桶与 AccessKey 凭据）；MinIO 官方 Windows 二进制下载已下线（`dl.min.io` 返回 410），因此本地桩件与不可达端点只能证明请求形状与失败分支，**不能证明与真实 OSS 的兼容性**。大文件（>64 MiB）分片路径、RAM 子账号最小权限策略、STS 临时凭据均未验证。

### MySQL 物理备份路线（Percona XtraBackup + binlog，2026-11-04）

- 背景：mysqldump 单线程、无压缩、单文件，几十 GB 的库既跑不完也恢复不动；`DBBAK_BACKUP_TIMEOUT` 默认 30 分钟会直接把大库备份杀掉。用户决定新增 MySQL 物理备份适配器，方案为 MySQL Community + Percona XtraBackup + binlog。
- 适配器路由：`Registry` 改为按「引擎 + 路线」索引，新增可选接口 `RouteNamer`（`Method() string`）；`ForRoute` 显式选择，未实现路线**报错而不回退**默认路线。`ForEngine` 仍返回默认路线，MySQL 默认仍是 `logical`（装配顺序保证）。`BackupSpec` 新增 `Base`，把恢复链基线交给适配器，避免执行器与适配器各自判断基线。
- 新增 `internal/backup/mysqlphysical.go`（`MySQLPhysicalClient`）：
  - 全量：`xtrabackup --backup --stream=xbstream` 输出**单个** `.xbstream`，复用现有哈希、FTP/OSS 上传与清理；`--extra-lsndir` 留存 checkpoints 与 `xtrabackup_binlog_info`，把 `to_lsn`/`binlog_file`/`binlog_pos`/GTID 写入 `engine_metadata`，固定标 `prepared=false` 并附恢复步骤。
  - binlog：`backup_type=log` 时按「上一份对象终点 → 当前位点」复制 `mysqlbinlog --read-from-remote-server --raw`，打包为单个 `.tar`；基线文件已被清除时返回 `BINLOG_PURGED` 并说明链断点，不静默跳过。
  - 前提校验：连接、`datadir` 是否本机可见、XtraBackup 与 MySQL 主版本对齐、`log_bin`/`binlog_format`。
  - 凭据：写 0600 临时 `[client]` 选项文件 + `--defaults-extra-file`，密码不进命令行与环境变量；拒绝含换行的账号密码。
  - 错误分类：`BACKUP_TIMEOUT`/`TOOL_MISSING`/`AUTH_FAILED`/`NETWORK_ERROR`/`PREREQUISITE_MISSING`/`TOOL_INCOMPATIBLE`/`STORAGE_FULL`/`BACKUP_OUTPUT_EMPTY`，不合并成「失败」。
  - 明确不做：`differential` 返回 `BACKUP_TYPE_UNSUPPORTED`（XtraBackup 增量需逐级 checkpoints 与逐级 prepare，元数据模型未承载，不用重复全量冒充）；不启动 `--stop-never` 常驻归档；备份内不做 `--prepare`。
- 迁移 9：`jobs.backup_method`、`artifacts.backup_method`；历史对象按 `method` 回填（`pg_basebackup`/`xtrabackup`/`mysqlbinlog` → `physical`，`sqlserver_native` → `native`，其余 `logical`），避免历史数据被误标。恢复链基线查询按 `backup_method` 过滤，逻辑与物理产物不能互为基线。
- 配置：`DBBAK_XTRABACKUP`（默认 `xtrabackup`）、`DBBAK_MYSQLBINLOG`（默认 `mysqlbinlog`）、`DBBAK_MYSQL_PHYSICAL_ENABLED`（默认关闭）。`DBBAK_BACKUP_TIMEOUT` 默认由 30 分钟改为 **6 小时**：原默认值在大库上必然超时并杀掉工具进程。
- API：任务接口新增 `backup_method` 校验（仅 MySQL 可用；`log` 仅 SQL Server 或 MySQL 物理路线；物理路线要求开关开启且非 Windows）。能力矩阵改为按路线逐条输出，新增 `route` 字段，方法名区分 `mysqldump`/`xtrabackup`/`mysqlbinlog`；Windows 上物理路线状态为 `unsupported` 而不是 `prerequisite_missing`。
- 前端：任务表单新增「备份路线」（默认逻辑 / 逻辑 mysqldump / 物理 XtraBackup），非 MySQL 隐藏并置空；备份类型可选项随引擎与路线联动，`log` 文案在物理路线下改为「binlog 归档」；任务列表标注「物理 XtraBackup」。
- 验证：`go build ./...`、`go vet ./...`、`go test ./...` 全绿；`node --check web/app.js` 通过。新增 9 个用例：路线解析与默认路线、未实现路线拒绝、缺工具分类、差异拒绝、binlog 起点解析（日志接日志/日志接全量/缺位点）、`xtrabackup_binlog_info` 三种格式、版本对齐解析、凭据文件转义与清理、`datadir` 非同机拒绝；API 侧新增 `TestJobBackupRouteValidation`（默认路线可建、逻辑路线拒绝 log、未启用拒绝物理、Windows 拒绝物理）。
- **未运行**：开发环境为 Windows，Percona XtraBackup 无 Windows 二进制，因此**从未执行过一次真实物理备份、`--prepare`、隔离恢复或 binlog 重放**。该路线能力状态固定 `unverified`，开关默认关闭。验证步骤见 `DBBak_文档/DBBak_MySQL物理备份验证说明.md`。

### Windows 安装包与原生服务（2026-11-04）

- 移除 WinSW 包装器（`deploy/windows/install.ps1`、`uninstall.ps1`、`dbbak-service.xml`）：服务进程改为 `dbbak.exe` 自身，用 `golang.org/x/sys/windows/svc` 实现，`svc.IsWindowsService()` 区分服务与控制台上下文，避免第三方向导 exe 与其 SHA 固定策略。
- `serve` 新增 `--addr`、`--data-dir`、`--backup-root`、`--master-key-file`，优先级为「命令行 > `DBBAK_*` 环境变量 > 默认值」；服务把这些固定路径写入 ImagePath，不再依赖注册表环境变量或 XML。
- 新增 `setup`：提权（`runas` 自拉起）、目录创建、复制程序、注册服务（延迟自动启动 + `sc failure` 重启策略）、按服务名授权 `NT SERVICE\DBBak` 的 ACL、初始化管理员（未指定密码时生成 24 位随机密码且只打印一次）、启动并轮询 `/health/ready`（90s，失败输出日志尾部）、写“设置 → 应用”卸载登记、打开浏览器。服务注册后的任一步失败都会回滚服务注册。
- 新增 `uninstall`（默认保留数据；`--purge` 必须同时 `--yes` 且目录名含 `dbbak` 才递归删除）与 `service status|start|stop|restart`。
- 新增服务日志：`<数据目录>\logs\dbbak.log`，10 MiB 轮转保留 5 份，先轮转后写入，避免留下空文件。
- 新增 `packaging/build-installer.ps1`：跑 vet/test → 构建 windows/amd64 → 校验产物版本与包名一致 → 用 Inno Setup 编译 `packaging/dbbak.iss` → 产出 `DBBak-Setup-<版本>-windows-amd64.exe`、便携 ZIP 与 `SHA256SUMS`。安装包为简体中文图形向导：选择安装目录、选择数据目录、设置监听地址与管理员密码，再由 `dbbak.exe setup` 注册服务。Inno Setup 7.1.1 缺失时自动下载并校验 SHA-256。
- 修复损坏命令名：7 处 `Read-Host` 被写成不存在的 cmdlet（含 `scripts/start-dev.ps1` 的密码提示，运行时会直接报错），已全部修回。
- 验证：`go build ./...`、`go vet -p=1 ./...`、`go test ./...` 全绿（新增 7 个用例：参数优先级、多余参数拒绝、管理员初始化幂等、路径越界拒绝、默认计划、随机密码字符集、日志轮转上限）；`setup --dry-run` 输出正确计划；前台 `serve --addr 127.0.0.1:38099` 实测 `/health/live`、`/health/ready` 均 200，首页 7152 字节；非管理员执行 `service status` 给出明确提权提示。
- 未验证：真实安装、ACL 授权、服务启动/停止/卸载全部需要管理员权限，本机当前会话无提权，**未执行**；`NT SERVICE\DBBak` 虚拟账号在 ACL 中的名称解析依赖服务已注册（实现已按“先注册服务再授权”排序），仍需在目标机确认。

### 服务账号看不到 MySQL 客户端（2026-11-05）

- 现象：已安装的 Windows 服务（`C:\Program Files\DBBak`，`127.0.0.1:8080`）上点「连接测试」返回 `TOOL_MISSING`。
- 根因：服务以虚拟账号 `NT SERVICE\DBBak` 运行，只继承**机器级**环境；`DBBAK_MYSQL` / `DBBAK_MYSQLDUMP` 从未写进注册表（安装器只写 ImagePath 参数），机器 `Path` 里也没有 MySQL 客户端，于是回落到 `mysqldump`，并在服务账号上下文中找不到。
- 错误信息不可操作：原 `TOOL_MISSING` 文案固定写成“请检查 DBBAK_MYSQL 与 DBBAK_MYSQLDUMP”，既不说缺哪个工具，也不说服务解析到哪个路径，且原始错误没进日志。
- 修复：
  - `internal/backup`：`ToolError` 增加只进日志的 `Detail`；`TOOL_MISSING` 文案改为指名缺失工具（`filepath.Base(path)`），不再固定提 mysql/mysqldump（PostgreSQL、SQL Server 同样受益）；新增 `DiagnosticError`。
  - `internal/api`：连接测试失败把工具路径与原始 stderr 写服务日志；能力矩阵在工具缺失时把服务实际使用的路径带进提示。
  - `cmd/dbbak setup`：新增 `--mysql-path`，写入机器级 `DBBAK_MYSQL`、`DBBAK_MYSQLDUMP`，并把 bin 目录追加到机器 `Path`（环境在注册服务前写入并广播 `WM_SETTINGCHANGE`）。
  - 文档：`DBBak_构建发布运行指南.md` 增加「服务账号与环境（必读）」；`DBBak_部署说明.md` 修复被合并成一行的环境变量表。
- 验证：`go build ./...`、`go vet ./cmd/dbbak`、`go test ./...` 全绿；新增 `TestMissingToolMessageCarriesConfiguredPath`（断言提示既点名 `mysqldump` 又带出配置路径，并校验执行器状态为 `prerequisite_missing`）。
- 未验证：`--mysql-path` 的真实安装写入需管理员权限，**未执行**；目标机需卸载后带 `--mysql-path` 重装，或在机器环境补 `DBBAK_*` / `Path` 后重启服务，才能复测连接测试。

### 装完即用：安装包自带客户端工具（2026-11-05）

- 目标：客户在自己电脑上装完直接可用，不配 `DBBAK_*`、不改 PATH、不另装 MySQL 客户端。
- 运行时查找顺序（`internal/toolenv` + `config.Load`）：`DBBAK_*` 显式配置 → 程序目录 `tools`（也认当前工作目录的 `tools`）→ 命令名交给系统 PATH。`mysql.exe`、`mysqldump.exe`、`mysqlbinlog.exe` 均走这条规则。
- PATH 补充（`Config.ApplySearchPath`）：服务启动时把存在的 `tools` 目录追加到进程 PATH 头部，并追加 `C:\Program Files\Common Files\Microsoft Shared\OLE DB\OLEDB\148`（sqlcmd 的 OLE DB 组件常不在服务账号 PATH 里）；只影响本进程，不改系统环境。
- 打包：`packaging/collect-tools.ps1` 只拷 `mysql.exe`、`mysqldump.exe`、`mysqlbinlog.exe`、`libcrypto-3-x64.dll`、`libssl-3-x64.dll`（约 29 MB，依赖来自三个 exe 导入表的实际读取），附 `LICENSE-mysql.txt` 与 `README-tools.txt`，复制后跑 `mysqldump --version` 冒烟，失败即构建失败。`build-installer.ps1` 新增 `-MySQLClientBin`（换版本）与 `-NoBundledTools`（内网自备客户端）。
- 安装器改为整目录安装：新增 `/DAppDir` 指向暂存区（`dbbak.exe` + `backup.ps1` + `tools`），`[Files]` 递归安装；`setup` 自带 `installBundledTools`，从自身所在目录的 `tools` 复制到安装目录。
- 重装即修复：删除“服务已存在就拒绝”，改为覆盖重装。`setup` 新增 `--repair` 语义（`setupOptions.Repair`）：停服 → 刷新 ImagePath → 跳过 ACL 重设 → 仅在给了密码时初始化管理员 → 启动；失败不回滚（不删客户机器上的服务）。向导预填已登记的数据目录、监听地址与管理员用户名，重装时密码可留空表示不重置。`ownService()` 防止覆盖同名但他人的服务。
- 停服时机修正（2026-11-05）：初版把 `backup.ps1` 停服放在 `InitializeSetup`，那里 `{app}` 尚未初始化，安装包一启动就报 `An attempt was made to expand the "app" constant before it was initialized`。改为在 `CurStepChanged(ssInstall)` 执行（常量已就绪、文件尚未复制），停服脚本目录由新增的 `GetServiceDir()` 从注册表 `ImagePath` 解析（能处理带引号、带参数、含空格的路径），避免用户改安装目录后停错服务；旧目录缺 `backup.ps1` 时回落 `sc stop DBBak`。`WizardSilent()` 下跳过确认框，使 `/VERYSILENT` 批量升级不被对话框挡死。
- 对话框按钮修正（2026-11-05）：启动确认框写的是 `MB_OKCANCEL` 却拿 `IDYES` 判断，点「确定」返回 `IDOK`，条件不成立，安装器静默退出（日志只有 `InitializeSetup returned False; aborting.`）。改为 `MB_OKCANCEL or MB_DEFBUTTON2` 配 `= IDCANCEL`（默认焦点在「取消」，误按回车不会停服），取消时额外提示「已取消安装，未做任何更改」。
- 旧脚本兼容（2026-11-05）：安装器原来直接把 `-StopService -DataRoot` 传给**已安装目录里的** `backup.ps1`，而那份是升级前的旧版本，没有这两个参数，会以“找不到参数”失败、服务停不下来，随后覆盖 `dbbak.exe` 被占用。改为安装器自己 `net stop DBBak`（退出码 2 视为本来已停），快照只传 `-Yes`：旧脚本此时已满足“服务已停”的前提，新脚本的数据目录会自动读注册表，两版都能用。另修 `installBundledTools` 在源与目标同目录时把文件复制到自身的问题（图形安装器已解压 `tools`，此时直接返回）。
- `backup.ps1` 配套改动（2026-11-05）：原脚本要求服务已停止（`throw`），直接由安装器调用必然失败，且 `DataRoot` 写死 `C:\ProgramData\DBBak`，自定义数据目录会找不到库。新增 `-StopService`（先 `Stop-Service` 并 `WaitForStatus('Stopped')`）与 `-DataRoot`（缺省读 `HKLM\Software\DBBak\DataDir`，再回落默认值）；安装器改传 `-Yes -StopService -DataRoot <已登记目录>`。原有“不带参数必须已停服”的保护语义保留。
- 错误提示：`TOOL_MISSING` 现在点名具体工具（`filepath.Base(path)`），PostgreSQL / SQL Server 同样受益；能力矩阵在缺工具时带出服务实际使用的路径；连接测试失败把路径与原始 stderr 写服务日志。
- 验证：`go build ./...`、`GOOS=windows go build ./...`、`go vet -p=1 ./...`、`go test ./...` 全绿（新增 `toolenv` 2 例、`config` 3 例、能力提示 1 例）；实际构建安装包成功，暂存 `app\tools` 内三个工具均独立运行；在 PATH 只有 `C:\Windows\system32;C:\Windows`、无任何 `DBBAK_*` 的环境下启动暂存目录的 `dbbak.exe serve`，`/api/v1/capabilities` 返回 `mysql@logical = mysqldump Ver 8.4.13`、逻辑路线 `supported`。
- 常量时机验证（2026-11-05）：用同一份代码编出一个 `PrivilegesRequired=lowest` 的探测安装器（只复制一个占位文件、不碰服务），静默运行成功，日志记录 `svcdir=C:\Program Files\DBBak`（ImagePath 解析正确）、`app-at-ssinstall=C:\Users\...\DBBakProbeTest`（`{app}` 在 ssInstall 可正常展开）、`Installation process succeeded`；正式安装包低权限变体静默启动也不再报常量错误。
- 未验证：真实管理员权限下运行新安装包（覆盖重装、停服快照、tools 落盘）；全新 Windows 未装 VC++ 运行库时自带客户端能否启动；向导页的交互流程（改安装目录、取消确认框）未逐一点过。
- 许可证风险：MySQL Community 为 GPLv2，随包分发需保留许可证文本，是否构成衍生作品分发需法务确认；不接受时可用 `-NoBundledTools` 构建，改为客户机自备客户端。

### 工程结构

- 应用代码位于仓库根：`go.mod`、`cmd/`、`internal/`、`embed.go` 与前端目录 `web/` 同级，embed 可解析，构建命令无需切换目录。
- 同步更新 `packaging/build.sh`、`packaging/build.ps1`、`deploy/docker/Dockerfile`、`README.md`、`AGENTS.md`、`DBBak_文档/` 内的路径与文档链接。
- 前端目录由 `DBBak_交互原型/` 更名为 `web/`，同步更新 embed 路径、`fs.Sub` 名称、静态资源测试与全部文档链接。
- 新增 `internal/api/static_test.go`：校验嵌入目录与 `fs.Sub` 名称一致，防止再次移动后静态资源静默失效。
- 验证：`go build ./...`、`go test ./...`、`go vet -p=1 ./...` 在仓库根全部通过；`packaging/build.sh` 实际产出 `dist/0.2.0-smoke/windows_amd64/dbbak.exe` 及校验和；重建后在 `127.0.0.1:38080` 实测首页 7184 字节、`app.js` 36121 字节均返回 200。
- 新增 `DBBak_文档/DBBak_构建发布运行指南.md`：构建、交叉编译、可复现打包、版本同步点、发布产物与校验和，以及 Windows 服务、Linux systemd、Docker 三种运行方式；未运行项逐节标注。
- 未验证：Docker 镜像构建（本机无 Docker）、迁移后未重跑真实数据库备份恢复脚本。

## 进行中

- 阶段三产品化：将已验证的 MySQL binlog/PostgreSQL WAL 接入任务、归档状态、恢复链和 PITR API。
- 阶段四剩余：Agent 控制端与 Runner 集成、日志链感知清理。

## 未完成或未运行

- SQL Server 恢复链的产品化缺口：无按时间点选择的恢复 UI，链成员不参与自动清理。
- MySQL binlog/PostgreSQL WAL 产品化调度、归档进程管理、恢复链 UI 和一键 PITR。
- Agent 控制端 `/agent/v1` 接口、Runner 执行集成、真实证书链和真实网络。
- 对象存储**真实阿里云 OSS 桶验收**（代码与桩件测试已完成，真实桶未跑）；NAS 仍依赖 UNC 路径或本地挂载。
- 日志链感知自动清理。
- Windows 服务安装/升级/卸载的**真实验收**（安装包与原生服务已实现，本机无提权未执行）、Linux systemd、Docker 构建与运行验证。
- 生产环境、大数据量、性能、RPO/RTO、完整故障注入和全浏览器矩阵。

## 下一步

1. 为 SQL Server 恢复链补产品化能力：按时间点选择恢复目标、链清单导出、链感知清理。
2. 将两套已通过 PITR 脚本的产品化接口、归档状态和恢复链模型接入 DBBak。
3. 完成 Agent 控制端及 Runner 集成，再接入日志链清理。
4. 在目标平台执行服务安装、升级、重启、持久化、恢复和卸载验收。
