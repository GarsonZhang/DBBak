# Vault 中文 Web 前端（web/）

本目录是 Vault 的生产前端源码，沿用原交互原型的主要布局，页面数据全部来自真实 Go API，不再使用样例数组或 localStorage 任务配置。

## 运行方式

前端由 Vault 服务通过 Go embed 统一提供，不需要 Node.js 或单独构建。构建需在仓库根目录执行：

```powershell
Push-Location .\src
go build -trimpath -o ..\.test-runtime\vault.exe .\cmd\vault
Pop-Location
.\.test-runtime\vault.exe serve
```

访问 `http://127.0.0.1:8080/`。初始化管理员和完整配置见根目录 `README.md` 与 `Vault_文档/Vault_部署说明.md`。

直接用 `file://` 打开 `index.html` 不属于真实服务运行方式；页面需要同源 API、HttpOnly 会话 Cookie 和 `X-Vault-Request` 同源确认头。

## 当前真实能力

- 管理员登录、退出和会话失效处理。
- MySQL 数据源新增、编辑和真实连接测试。
- PostgreSQL 维护库连接、REPLICATION/max_wal_senders 前提检测和整实例物理全量。
- 本地授权目录新增、编辑和写权限检查。
- MySQL 全量任务、每日/每周/分钟调度、手动执行和状态轮询。
- SQLite 运行记录、错误日志、命令/文件/恢复三个验证等级。
- 备份文件大小、SHA-256、工具版本和人工锁定。
- 保留策略、服务端清理预览与明确确认后的清理。
- hash 深链和浏览器前进/后退导航。

SQL Server 适配器与界面入口已实现（原生 BACKUP DATABASE 全量），已完成真实备份与异名恢复验收；任务创建需服务端开启 `VAULT_SQLSERVER_ENABLED`。差异/增量、事务日志/binlog/WAL、远程 Agent 控制端、NAS/S3 和正式三平台发行包仍未实现。PostgreSQL 当前只实现 Windows 原生 pg_basebackup 全量，pgBackRest 和 WAL 时间点恢复未实现。

## 文件

- `index.html`：导航、图标与应用入口。
- `style.css`：原型的响应式视觉样式，仍含可回退的 Google Fonts 引用。
- `app-extra.css`：真实登录、错误和运行状态补充样式。
- `app.js`：真实 API、表单、渲染和交互逻辑。
- `DESIGN.md`：原始设计与后续生产路线说明。

密码只在登录或数据源表单提交时进入请求体，不写入 localStorage。静态资源由后端嵌入，修改本目录后需要重新构建 Vault 二进制才能反映到嵌入版本。
