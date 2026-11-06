# DBBak 阶段一验证报告

日期：2026-10-30 · 版本：0.1.0-phase1 · 范围：MySQL 真实全量备份闭环

## 结论

在当前 **Windows x86-64 + 本地执行器 + 隔离 MySQL 8.4.13 LTS** 组合下，阶段一核心真实闭环已验证：管理员认证、数据源与目录、SQLite 持久化、手动和后台调度、真实备份文件、SHA-256、运行记录、隔离恢复、清理预览和中文 API 页面均可运行。

本报告不代表其他数据库、Linux、Docker、远程 Agent、生产环境或正式发行包已验收。

## 实际实现

- Go 单实例服务、环境配置、SQLite WAL 与版本化迁移。
- 管理员初始化、bcrypt 密码哈希、HttpOnly 会话、接口鉴权和写请求同源确认。
- AES-GCM 数据源凭据加密，主密钥与 SQLite 分离。
- MySQL 数据源新增/编辑、真实连接测试、本地目录新增/编辑和真实写检查。
- mysqldump 单库逻辑全量、唯一临时文件、命令状态、文件状态、SHA-256 和工具版本记录。
- 每日/每周/分钟调度、时区内嵌、错过计划记录、幂等触发、活动运行唯一约束和重启对账。
- 运行日志、备份文件、人工锁定、保留预览、预览版本/指纹复查和授权目录内单文件删除。
- 隔离恢复结果登记；恢复验证与命令成功、文件校验分开保存。
- 现有中文布局接入真实 API，包含登录、数据源、存储、任务、日志、清理和系统设置。

## 关键代码与交付物

- `cmd/dbbak/`：init-admin、serve、version。
- `internal/api/`：REST API、认证、静态资源和安全响应头。
- `internal/backup/`：MySQL 客户端、执行器、调度、路径约束和清理。
- `internal/metadata/`：SQLite 迁移、持久化和并发约束。
- `internal/security/`：密码、会话和凭据加密。
- `web/`：真实 API 前端，由 Go 嵌入。
- `scripts/`：便携 MySQL 初始化、真实备份恢复和后台调度验证。
- `deploy/test/`：MySQL 8.4.13 隔离 Compose。
- `deploy/docker/Dockerfile`：阶段一 Linux 构建文件，未运行。
- `README.md`、`DBBak_API.md`、`DBBak_部署说明.md`、`DBBak_兼容性矩阵.md`、`TASKS.md`。

## 已运行验证

### 1. Go 自动化测试与静态检查

执行：

```powershell
go test ./...
go vet -p=1 ./...
go test -cover ./...
```

结果：全部通过。覆盖率为 API 30.2%、backup 48.3%、metadata 27.0%、security 71.1%；入口和 config 当前无直接覆盖率，不能据此宣称完整覆盖。

覆盖场景包括：

- SQLite 重启持久化、幂等键和活动运行冲突。
- 同一任务并发防重、真实文件与元数据一致、三个验证等级分离。
- 每日/每周/分钟时区计算和夏令时不存在时刻跳过。
- 缺失工具返回 TOOL_MISSING，错误消息密码脱敏。
- Windows/UNC/绝对路径与 `..` 越界拒绝。
- 隔离临时目录内过期文件删除、最低份数保护、预览过期/状态变化拒绝。
- API 未认证 401、Cookie 写请求缺少确认头 403、数据源/仓库/任务持久化。

### 2. 真实 MySQL 备份与隔离恢复

环境：Windows x86-64，Go 1.27.3，MySQL Server/mysql/mysqldump 均 8.4.13，数据库仅监听 127.0.0.1:33306。

执行：

```powershell
python scripts/phase1_api_verify.py
```

结果：通过。

- 运行 ID：`run_2ecd4718660fe0f1661a0096`。
- 真实文件：`.test-runtime/backup-root/dbbak_source/2026/10/run_2ecd4718660fe0f1661a0096.sql`。
- 大小：2296 字节。
- SHA-256：`b62cda6d0847b1787e822f12e641ea240a86f397af949a0410abcf8555e30898`。
- 转储包含预期 sample_records 表结构和中文样本。
- 恢复到独立 dbbak_restore，核对 4 个字段、3 行、amount 合计 601.25，中文 A/B/C 三条内容一致。
- restore_status 已登记为 verified。
- 错误密码真实连接返回 AUTH_FAILED；普通文件作为备份根目录返回 STORAGE_NOT_WRITABLE。
- 保留窗口内备份预览候选为 0，没有删除受保护备份。

### 3. 服务重启与无浏览器调度

执行：

```powershell
python scripts/phase1_schedule_verify.py
```

结果：通过。重启后保留 2 个数据源（含负向测试源）、1 个任务、运行与恢复验证记录；本次服务启动后产生成功计划运行 `run_617a73999b258534d1b62692`。验证期间没有打开浏览器，调度由服务进程执行。

### 4. 中文前端

使用真实 Chrome 渲染并登录：

- 登录页中文、表单和布局正常。
- 概览、任务、数据库和清理页面均显示真实 API 数据。
- 验证任务列表、每分钟计划、数据源名称、备份文件与清理入口。
- 捕获 JavaScript 异常数量：0。
- 嵌入 index.html、app.js、app-extra.css 均返回 HTTP 200，/health/ready 返回 ready。

本次不是全浏览器、全分辨率或完整可访问性验收。

### 5. 构建与脚本

- Windows amd64 可执行文件构建成功，版本 0.1.0-phase1。
- Linux amd64 交叉构建成功，CGO_ENABLED=0。
- 三个 Python 验证/初始化脚本通过 py_compile。
- Go vet 初次遇到 Windows vet.exe 并发文件占用，改为 `-p=1` 后真实通过；未隐藏初次错误。
- 本次维护的 58 个文本文件已检查为 CRLF 且无尾随空白；原始导出 `style.css` 保留其 LF 行尾，未为格式统一重写第三方资源。

## 未运行、未验证和限制

- Docker 未安装或不在 PATH：Compose 未启动，Dockerfile 未构建，容器持久化未验证。
- Linux 仅交叉编译，未启动、未运行 systemd、未做真实备份恢复。
- Windows 服务、安装包、签名、升级、卸载未实现。
- 远程 Agent、NAS、自动清理、自动恢复执行未实现。（对象存储 `kind=oss` 为后续新增能力，见 TASKS.md，不受本报告范围约束。）
- SQL Server、PostgreSQL、差异/增量、事务日志/binlog/WAL 和时间点恢复未实现。
- 未进行生产数据、大数据量、性能、RPO/RTO、断网、磁盘满和跨平台完整故障验收。
- 当前恢复验证通过脚本执行并登记，API 不会自动执行恢复。
- 清理实删验证在 Go 隔离临时目录完成；未对任何生产备份执行删除。

## 下一阶段任务

1. 保持阶段一真实回归测试，先冻结 SQL Server 或 PostgreSQL 的一个最小版本/系统/工具组合。
2. 实现对应真实全量适配器、能力探测和文件/路径语义。
3. 在独立目标完成逐引擎恢复验证后，再扩展兼容矩阵。
4. 不因接口或构建文件已经存在而提前宣称后续阶段能力。
