
FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.27.3-bookworm AS build
# 构建阶段默认使用国内模块代理：容器内没有宿主机的 GOPROXY 配置，
# 直连 proxy.golang.org 在国内网络会 dial tcp 超时导致 go mod download 失败。
# 不追加 direct：镜像缺失模块时立即报错，避免回退到 VCS 拉取再超时。
# 需要走企业私服或官方源时用 --build-arg GOPROXY=... 覆盖。
ARG GOPROXY=https://goproxy.cn
ENV GOPROXY=${GOPROXY}
# 只复制构建所需内容，避免把文档、测试数据和工具链带进镜像层。
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY embed.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/ ./web/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags='-s -w -buildid=' -o /out/dbbak ./cmd/dbbak

FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/mysql:8.4.13
LABEL org.opencontainers.image.version="0.2.0-phase5"
# SQL Server 适配器（连接探测、BACKUP DATABASE、msdb 元数据查询）全部通过 sqlcmd 执行。
# 微软的 mssql-tools 不随任何通用基础镜像分发，基础镜像只带 MySQL 客户端，缺了它
# SQL Server 路线会直接报 TOOL_MISSING。默认装入；只用 MySQL/PostgreSQL 的部署可
# 用 --build-arg INSTALL_SQLCMD=0 跳过，避免镜像白增约 100MB 和额外的微软许可义务。
ARG INSTALL_SQLCMD=1
# 基础镜像为 Oracle Linux 9，这里用微软 RHEL 9 源；该地址在国内可能很慢，
# 构建失败时先确认容器内能访问 packages.microsoft.com，而不是关掉校验。
ARG MSSQL_REPO_URL=https://packages.microsoft.com/config/rhel/9/prod.repo
RUN microdnf install -y curl && microdnf clean all \
    && groupadd --system --gid 10001 dbbak \
    && useradd --system --uid 10001 --gid 10001 --no-create-home --shell /sbin/nologin dbbak \
    && mkdir -p /var/lib/dbbak /backups /etc/dbbak-key \
    && chown -R 10001:10001 /var/lib/dbbak /backups /etc/dbbak-key
# sqlcmd 单独一层：升级 MySQL 客户端或改动用户/目录时不必重装它。
# ACCEPT_EULA=Y 表示接受微软 SQL Server 命令行工具的许可条款，再分发前需单独审查；
# 装完用软链挂进 PATH，config.Load 未做 tools 目录解析，只按 PATH 查找 sqlcmd。
RUN if [ "$INSTALL_SQLCMD" = "1" ]; then \
        curl -fsSL "$MSSQL_REPO_URL" -o /etc/yum.repos.d/mssql-release.repo \
     && ACCEPT_EULA=Y microdnf install -y msodbcsql18 mssql-tools18 \
     && microdnf clean all \
     && rm -rf /var/cache/yum /var/cache/dnf \
     && ln -sf /opt/mssql-tools18/bin/sqlcmd /usr/local/bin/sqlcmd; \
    fi
COPY --from=build --chown=10001:10001 /out/dbbak /usr/local/bin/dbbak
ENV DBBAK_ADDR=0.0.0.0:8080 \
    DBBAK_DATA_DIR=/var/lib/dbbak \
    DBBAK_DATABASE=/var/lib/dbbak/dbbak.db \
    DBBAK_BACKUP_ROOT=/backups \
    DBBAK_MASTER_KEY_FILE=/etc/dbbak-key/master.key \
    DBBAK_MYSQL=mysql \
    DBBAK_MYSQLDUMP=mysqldump
VOLUME ["/var/lib/dbbak", "/backups", "/etc/dbbak-key"]
EXPOSE 8080
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/dbbak"]
CMD ["serve"]
