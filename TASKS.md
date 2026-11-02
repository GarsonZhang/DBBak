# Vault 阶段任务清单

更新时间：2026-11-02

## 当前状态

阶段一 MySQL 全量、阶段二 PostgreSQL 全量与 SQL Server 原生全量均已真实验收（SQL Server 于 2026-11-02 完成异名恢复）。MySQL binlog、PostgreSQL WAL 时间点恢复脚本已真实验证，但尚未接入产品调度。远程 Agent 协议、自动清理和发行脚本有实现或测试，仍有控制端、S3、服务运行和 Docker 验收缺口。

## 技术基线

- 服务端：Go 1.27.3，单实例模块化单体。
- 元数据：SQLite WAL，modernc.org/sqlite v1.61.0。
- 安全：bcrypt、HttpOnly 会话、AES-GCM 独立主密钥、Agent mTLS。
- 已验证数据库：MySQL 8.4.13 LTS；PostgreSQL 18.6。
- 前端：现有中文布局接真实 API，由 Go embed 提供。
- 代码布局：Go 模块根在仓库根目录（`go.mod`、`cmd/`、`internal/`、`embed.go`、`web/`）；`deploy/`、`packaging/`、`scripts/`、`Vault_文档/` 并列存放。
- 测试边界：.test-runtime 隔离实例、独立恢复目标和测试目录。

## 已完成并验证

### 阶段一与二

- Go 服务、SQLite 迁移、认证、凭据加密和审计。
- 数据源、本地仓库、任务、调度、运行、文件、清理和恢复记录 API。
- MySQL 逻辑全量、SHA-256、真实备份和独立恢复通过。
- PostgreSQL 整实例 tar 物理全量、必要 WAL、system identifier 和独立集群恢复通过。
- 中文页面真实 API 接入及 Chrome 关键页面验证。

### 阶段三已验证脚本

- MySQL 8.4.13 binlog：全量记录真实坐标、ROW/FULL、FLUSH LOGS、隔离 vault_restore、目标标记与目标后标记重放边界通过。
- PostgreSQL 18.6 WAL：archive_mode、目标 LSN、真实归档段重放、目标后事务排除、system identifier 一致。
- 脚本均有哈希证据和明确超时；MySQL 与 PostgreSQL 源/恢复实例保持隔离。

### 阶段四与五已实现部分

- 远程 Agent：mTLS 客户端、一次性注册、CSR、fence、递增序号、去重、有界重试；单元和 httptest 通过。
- 独立全量自动清理：每天 05:00 后运行，复用预览、指纹、路径、哈希和最低份数保护；测试通过。
- Windows WinSW、Linux systemd、Docker/Compose、跨平台构建脚本已交付并通过静态检查。

### 工程结构

- 应用代码位于仓库根：`go.mod`、`cmd/`、`internal/`、`embed.go` 与前端目录 `web/` 同级，embed 可解析，构建命令无需切换目录。
- 同步更新 `packaging/build.sh`、`packaging/build.ps1`、`deploy/docker/Dockerfile`、`README.md`、`AGENTS.md`、`Vault_文档/` 内的路径与文档链接。
- 前端目录由 `Vault_交互原型/` 更名为 `web/`，同步更新 embed 路径、`fs.Sub` 名称、静态资源测试与全部文档链接。
- 新增 `internal/api/static_test.go`：校验嵌入目录与 `fs.Sub` 名称一致，防止再次移动后静态资源静默失效。
- 验证：`go build ./...`、`go test ./...`、`go vet -p=1 ./...` 在仓库根全部通过；`packaging/build.sh` 实际产出 `dist/0.2.0-smoke/windows_amd64/vault.exe` 及校验和；重建后在 `127.0.0.1:38080` 实测首页 7184 字节、`app.js` 36121 字节均返回 200。
- 新增 `Vault_文档/Vault_构建发布运行指南.md`：构建、交叉编译、可复现打包、版本同步点、发布产物与校验和，以及 Windows 服务、Linux systemd、Docker 三种运行方式；未运行项逐节标注。
- 未验证：Docker 镜像构建（本机无 Docker）、迁移后未重跑真实数据库备份恢复脚本。

## 进行中

- 阶段三产品化：将已验证的 MySQL binlog/PostgreSQL WAL 接入任务、归档状态、恢复链和 PITR API。
- 阶段四剩余：Agent 控制端与 Runner 集成、S3 存储、日志链感知清理。

## 未完成或未运行

- SQL Server 恢复链的产品化缺口：无按时间点选择的恢复 UI，链成员不参与自动清理。
- MySQL binlog/PostgreSQL WAL 产品化调度、归档进程管理、恢复链 UI 和一键 PITR。
- Agent 控制端 `/agent/v1` 接口、Runner 执行集成、真实证书链和真实网络。
- S3 对象存储；NAS 仍依赖本地/挂载路径。
- 日志链感知自动清理。
- Windows 服务安装/升级/卸载、Linux systemd、Docker 构建与运行验证。
- 生产环境、大数据量、性能、RPO/RTO、完整故障注入和全浏览器矩阵。

## 下一步

1. 为 SQL Server 恢复链补产品化能力：按时间点选择恢复目标、链清单导出、链感知清理。
2. 将两套已通过 PITR 脚本的产品化接口、归档状态和恢复链模型接入 Vault。
3. 完成 Agent 控制端及 Runner 集成，再接入 S3 和日志链清理。
4. 在目标平台执行服务安装、升级、重启、持久化、恢复和卸载验收。
